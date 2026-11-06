package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

const (
	cloudID = "1eb9eb54-74a9-4d94-8eb1-a1a7696be552" // bloqit.atlassian.net
	site    = "https://bloqit.atlassian.net"
)

type Issue struct {
	Key      string
	Summary  string
	Status   string
	Category string
	Assignee string
	Priority string
	Type     string
}

// Board column order; statuses not listed sort after these by category.
var statusOrder = []string{"in progress", "blocked", "in review", "testing", "staging", "ongoing", "ready for engineering", "to do", "refining", "triage", "backlog"}
var categoryOrder = map[string]int{"In Progress": 0, "To Do": 1, "Done": 2}

func rank(i Issue) int {
	if at := slices.Index(statusOrder, strings.ToLower(i.Status)); at >= 0 {
		return at
	}
	c, ok := categoryOrder[i.Category]
	if !ok {
		c = 9
	}
	return len(statusOrder) + c
}

// localeCompare approximates JS's String.localeCompare: case-insensitive first, then exact.
func localeCompare(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// Jira connects on first use, so the board can draw before the MCP session is up. Concurrent
// callers share one connection attempt; a failed attempt is retried by the next call.
type Jira struct {
	mu  sync.Mutex
	mcp *Mcp
	log func(string)
}

func (j *Jira) conn(ctx context.Context) (*Mcp, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.mcp == nil {
		m, err := connect(ctx, j.log)
		if err != nil {
			return nil, err
		}
		j.mcp = m
	}
	return j.mcp, nil
}

func (j *Jira) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.mcp != nil {
		_ = j.mcp.close()
	}
}

func (j *Jira) call(ctx context.Context, tool string, args map[string]any, out any) error {
	m, err := j.conn(ctx)
	if err != nil {
		return err
	}
	args["cloudId"] = cloudID
	return m.call(ctx, tool, args, out)
}

type named struct {
	Name string `json:"name"`
}

type issueFields struct {
	Summary string `json:"summary"`
	Status  *struct {
		Name           string `json:"name"`
		StatusCategory *named `json:"statusCategory"`
	} `json:"status"`
	Assignee *struct {
		DisplayName string `json:"displayName"`
	} `json:"assignee"`
	Priority    *named   `json:"priority"`
	IssueType   *named   `json:"issuetype"`
	Description string   `json:"description"`
	Labels      []string `json:"labels"`
	Parent      *struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
		} `json:"fields"`
	} `json:"parent"`
	IssueLinks []struct {
		Type *struct {
			Inward  string `json:"inward"`
			Outward string `json:"outward"`
		} `json:"type"`
		InwardIssue  *linkedIssue `json:"inwardIssue"`
		OutwardIssue *linkedIssue `json:"outwardIssue"`
	} `json:"issuelinks"`
}

type linkedIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

type searchResult struct {
	Issues []struct {
		Key    string      `json:"key"`
		Fields issueFields `json:"fields"`
	} `json:"issues"`
	IsLast        bool   `json:"isLast"`
	NextPageToken string `json:"nextPageToken"`
}

func (j *Jira) issues(ctx context.Context, jql string) ([]Issue, error) {
	var issues []Issue
	next := ""
	for {
		args := map[string]any{
			"jql":        jql,
			"maxResults": 100,
			"fields":     []string{"summary", "status", "assignee", "priority", "issuetype"},
		}
		if next != "" {
			args["nextPageToken"] = next
		}
		var data searchResult
		if err := j.call(ctx, "searchJiraIssuesUsingJql", args, &data); err != nil {
			return nil, err
		}
		for _, i := range data.Issues {
			f := i.Fields
			is := Issue{Key: i.Key, Summary: f.Summary, Status: "?"}
			if f.Status != nil {
				is.Status = f.Status.Name
				if f.Status.StatusCategory != nil {
					is.Category = f.Status.StatusCategory.Name
				}
			}
			if f.Assignee != nil {
				is.Assignee = f.Assignee.DisplayName
			}
			if f.Priority != nil {
				is.Priority = f.Priority.Name
			}
			if f.IssueType != nil {
				is.Type = f.IssueType.Name
			}
			issues = append(issues, is)
		}
		next = data.NextPageToken
		if data.IsLast || next == "" || len(issues) >= 500 {
			break
		}
	}
	slices.SortStableFunc(issues, func(a, b Issue) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		if c := localeCompare(a.Assignee, b.Assignee); c != 0 {
			return c
		}
		return localeCompare(a.Key, b.Key)
	})
	return issues, nil
}

func (j *Jira) me(ctx context.Context) string {
	var data searchResult
	err := j.call(ctx, "searchJiraIssuesUsingJql", map[string]any{
		"jql": "assignee = currentUser() ORDER BY updated DESC", "maxResults": 1, "fields": []string{"assignee"},
	}, &data)
	if err != nil || len(data.Issues) == 0 || data.Issues[0].Fields.Assignee == nil {
		return ""
	}
	return data.Issues[0].Fields.Assignee.DisplayName
}

// detail is the ticket as markdown: header, description, linked issues.
func (j *Jira) detail(ctx context.Context, key string) (string, error) {
	var data struct {
		Fields issueFields `json:"fields"`
	}
	err := j.call(ctx, "getJiraIssue", map[string]any{
		"issueIdOrKey":          key,
		"fields":                []string{"summary", "description", "status", "assignee", "priority", "issuetype", "labels", "parent", "issuelinks"},
		"responseContentFormat": "markdown",
	}, &data)
	if err != nil {
		return "", err
	}
	f := data.Fields

	var links []string
	for _, l := range f.IssueLinks {
		other, label := l.InwardIssue, ""
		if l.Type != nil {
			label = l.Type.Inward
		}
		if l.OutwardIssue != nil {
			other = l.OutwardIssue
			if l.Type != nil {
				label = l.Type.Outward
			}
		}
		if other == nil {
			continue
		}
		if label == "" {
			label = "relates to"
		}
		links = append(links, fmt.Sprintf("- %s %s: %s", label, other.Key, other.Fields.Summary))
	}

	var meta []string
	if f.IssueType != nil && f.IssueType.Name != "" {
		meta = append(meta, "Type: "+f.IssueType.Name)
	}
	if f.Status != nil && f.Status.Name != "" {
		meta = append(meta, "Status: "+f.Status.Name)
	}
	if f.Priority != nil && f.Priority.Name != "" {
		meta = append(meta, "Priority: "+f.Priority.Name)
	}
	assignee := "unassigned"
	if f.Assignee != nil {
		assignee = f.Assignee.DisplayName
	}
	meta = append(meta, "Assignee: "+assignee)

	parent, labels, linked := "", "", ""
	if f.Parent != nil {
		parent = fmt.Sprintf("Parent: %s %s", f.Parent.Key, f.Parent.Fields.Summary)
	}
	if len(f.Labels) > 0 {
		labels = "Labels: " + strings.Join(f.Labels, ", ")
	}
	if len(links) > 0 {
		linked = "\n## Linked issues\n\n" + strings.Join(links, "\n")
	}
	desc := f.Description
	if desc == "" {
		desc = "_No description._"
	}
	lines := []string{
		fmt.Sprintf("# %s: %s", key, f.Summary), "", site + "/browse/" + key,
		strings.Join(meta, " · "), parent, labels, "", desc, linked,
	}
	// Drop empty lines that follow another empty line.
	var out []string
	for n, l := range lines {
		if l != "" || n == 0 || lines[n-1] != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n"), nil
}
