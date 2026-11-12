package main

import (
	"context"
	"fmt"
	"strings"
)

// Jira writes: a ticket's status and assignee, changed only from the board's ^w and ^y. The
// MCP's own tools take them (transitionJiraIssue, editJiraIssue); the lists to pick from are
// operations behind its executeRead tool, named by its discover tool.

type Transition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   struct {
		Name           string `json:"name"`
		StatusCategory struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	} `json:"to"`
}

type JiraUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"emailAddress"`
	Active      bool   `json:"active"`
}

// categoryNames maps a status category's key, which transitions carry, to the name issues carry.
var categoryNames = map[string]string{"new": "To Do", "indeterminate": "In Progress", "done": "Done"}

// boardCategory is the category the board colours a status by.
func boardCategory(status, category string) string {
	// Jira's categories are only To Do, In Progress and Done; a blocked status is In Progress.
	if strings.Contains(strings.ToLower(status), "blocked") {
		return "Blocked"
	}
	return category
}

// executeRead runs one of the MCP's read operations that aren't tools of their own.
func (j *Jira) executeRead(ctx context.Context, name string, inputs map[string]any, out any) error {
	inputs["cloudId"] = cloudID
	return j.call(ctx, "executeRead", map[string]any{"name": name, "inputs": inputs}, out)
}

// transitions are the workflow moves the ticket can take now, for the signed-in user.
func (j *Jira) transitions(ctx context.Context, key string) ([]Transition, error) {
	var data struct {
		Transitions []Transition `json:"transitions"`
	}
	if err := j.executeRead(ctx, "listJiraIssueTransitions", map[string]any{"issueIdOrKey": key}, &data); err != nil {
		return nil, err
	}
	return data.Transitions, nil
}

// assignableUsers are the active users who can be assigned the ticket, matching query when set.
// Jira returns at most 50, so the picker searches as I type rather than filtering one list.
func (j *Jira) assignableUsers(ctx context.Context, key, query string) ([]JiraUser, error) {
	inputs := map[string]any{"issueKey": key}
	if query = strings.TrimSpace(query); query != "" {
		inputs["query"] = query
	}
	var users []JiraUser
	if err := j.executeRead(ctx, "listJiraIssueAssignableUsers", inputs, &users); err != nil {
		return nil, err
	}
	out := users[:0]
	for _, u := range users {
		if u.Active {
			out = append(out, u)
		}
	}
	return out, nil
}

// currentUser is the signed-in user.
func (j *Jira) currentUser(ctx context.Context) (JiraUser, error) {
	var u JiraUser
	err := j.executeRead(ctx, "getJiraCurrentUser", map[string]any{}, &u)
	return u, err
}

func transitionArgs(key, transitionID string) map[string]any {
	return map[string]any{"issueIdOrKey": key, "transitionId": transitionID}
}

// assignArgs sets the assignee to the account, or clears it for "".
func assignArgs(key, accountID string) map[string]any {
	var assignee any // JSON null clears the field
	if accountID != "" {
		assignee = map[string]any{"accountId": accountID}
	}
	return map[string]any{"issueIdOrKey": key, "fields": map[string]any{"assignee": assignee}}
}

// transition moves the ticket along a transition from transitions.
func (j *Jira) transition(ctx context.Context, key, transitionID string) error {
	return j.call(ctx, "transitionJiraIssue", transitionArgs(key, transitionID), nil)
}

// assign sets the ticket's assignee; "" unassigns it.
func (j *Jira) assign(ctx context.Context, key, accountID string) error {
	return j.call(ctx, "editJiraIssue", assignArgs(key, accountID), nil)
}

// issue reads one ticket in the board's shape.
func (j *Jira) issue(ctx context.Context, key string) (Issue, error) {
	list, _, err := j.issues(ctx, "key = "+key)
	if err != nil {
		return Issue{}, err
	}
	if len(list) == 0 {
		return Issue{}, fmt.Errorf("%s isn't in Jira", key)
	}
	return list[0], nil
}
