package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAtlassian is an MCP server in memory that records each call's arguments as JSON and
// answers from reply, keyed by tool, or by operation name for executeRead.
type fakeAtlassian struct {
	mu    sync.Mutex
	calls []fakeCall
	reply func(tool string, args map[string]any) (text string, isError bool)
}

type fakeCall struct {
	tool string
	args string // the arguments as sent, JSON
}

func (f *fakeAtlassian) sent(tool string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.tool == tool {
			out = append(out, c.args)
		}
	}
	return out
}

func newFakeJira(t *testing.T, reply func(tool string, args map[string]any) (string, bool)) (*Jira, *fakeAtlassian) {
	t.Helper()
	f := &fakeAtlassian{reply: reply}
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-atlassian"}, nil)
	for _, name := range []string{"executeRead", "transitionJiraIssue", "editJiraIssue", "searchJiraIssuesUsingJql"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				raw := string(req.Params.Arguments)
				f.mu.Lock()
				f.calls = append(f.calls, fakeCall{name, raw})
				f.mu.Unlock()
				var args map[string]any
				_ = json.Unmarshal(req.Params.Arguments, &args)
				text, isError := f.reply(name, args)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isError}, nil
			})
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "foreman"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return &Jira{mcp: &Mcp{session: cs}, log: func(string) {}}, f
}

// sameJSON fails unless got and want are the same JSON value.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("sent %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("sent\n  %s\nwant\n  %s", gb, wb)
	}
}

// Replies as Atlassian's MCP gave them (2026-11-12), trimmed.
const (
	transitionsReply = `{"data":{"transitions":[` +
		`{"id":"21","name":"In Progress","to":{"name":"IN PROGRESS","statusCategory":{"key":"indeterminate"}},"hasScreen":false,"isAvailable":true},` +
		`{"id":"101","name":"Send to review","to":{"name":"IN REVIEW","statusCategory":{"key":"indeterminate"}},"hasScreen":false,"isAvailable":true},` +
		`{"id":"201","name":"Blocked","to":{"name":"BLOCKED","statusCategory":{"key":"indeterminate"}},"hasScreen":false,"isAvailable":true},` +
		`{"id":"31","name":"Done","to":{"name":"DONE","statusCategory":{"key":"done"}},"hasScreen":false,"isAvailable":true}]}}`
	usersReply = `{"data":[` +
		`{"accountId":"acc-me","displayName":"Ricardo Ferreira","emailAddress":"me@example.com","active":true},` +
		`{"accountId":"acc-ana","displayName":"Ana Lopes","emailAddress":"","active":true},` +
		`{"accountId":"acc-gone","displayName":"Old Account","emailAddress":"","active":false}]}`
	meReply = `{"data":{"accountId":"acc-me","accountType":"atlassian","displayName":"Ricardo Ferreira","emailAddress":"me@example.com","active":true}}`
)

func TestTransitionPayload(t *testing.T) {
	// transitionJiraIssue's reply isn't decoded: a write that worked mustn't fail on its wording.
	j, f := newFakeJira(t, func(string, map[string]any) (string, bool) { return "Transitioned FLD-1 to IN REVIEW", false })
	if err := j.transition(context.Background(), "FLD-1", "101"); err != nil {
		t.Fatal(err)
	}
	sent := f.sent("transitionJiraIssue")
	if len(sent) != 1 {
		t.Fatalf("sent %d transitions, want 1", len(sent))
	}
	sameJSON(t, sent[0], `{"cloudId":"`+cloudID+`","issueIdOrKey":"FLD-1","transitionId":"101"}`)
}

func TestAssignPayload(t *testing.T) {
	j, f := newFakeJira(t, func(string, map[string]any) (string, bool) { return `{"key":"FLD-1"}`, false })
	ctx := context.Background()
	if err := j.assign(ctx, "FLD-1", "acc-ana"); err != nil {
		t.Fatal(err)
	}
	if err := j.assign(ctx, "FLD-1", ""); err != nil {
		t.Fatal(err)
	}
	sent := f.sent("editJiraIssue")
	if len(sent) != 2 {
		t.Fatalf("sent %d edits, want 2", len(sent))
	}
	sameJSON(t, sent[0], `{"cloudId":"`+cloudID+`","issueIdOrKey":"FLD-1","fields":{"assignee":{"accountId":"acc-ana"}}}`)
	// Unassigning sends an explicit null, which is how editJiraIssue clears a field.
	sameJSON(t, sent[1], `{"cloudId":"`+cloudID+`","issueIdOrKey":"FLD-1","fields":{"assignee":null}}`)
}

func TestWriteErrorIsReported(t *testing.T) {
	j, _ := newFakeJira(t, func(string, map[string]any) (string, bool) { return "Transition 999 is not valid", true })
	err := j.transition(context.Background(), "FLD-1", "999")
	if err == nil || !strings.Contains(err.Error(), "Transition 999 is not valid") {
		t.Fatalf("err = %v, want Jira's message", err)
	}
}

func TestPickListPayloads(t *testing.T) {
	j, f := newFakeJira(t, func(tool string, args map[string]any) (string, bool) {
		switch args["name"] {
		case "listJiraIssueTransitions":
			return transitionsReply, false
		case "listJiraIssueAssignableUsers":
			return usersReply, false
		case "getJiraCurrentUser":
			return meReply, false
		}
		return "unexpected " + tool, true
	})
	ctx := context.Background()
	ts, err := j.transitions(ctx, "FLD-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 4 || ts[1].ID != "101" || ts[1].To.Name != "IN REVIEW" || ts[1].To.StatusCategory.Key != "indeterminate" {
		t.Errorf("transitions = %+v", ts)
	}
	users, err := j.assignableUsers(ctx, "FLD-1", " ana ")
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[1].AccountID != "acc-ana" {
		t.Errorf("users = %+v, want the two active ones", users)
	}
	if _, err := j.assignableUsers(ctx, "FLD-1", ""); err != nil {
		t.Fatal(err)
	}
	me, err := j.currentUser(ctx)
	if err != nil || me.AccountID != "acc-me" || me.DisplayName != "Ricardo Ferreira" {
		t.Errorf("currentUser = %+v, %v", me, err)
	}

	sent := f.sent("executeRead")
	if len(sent) != 4 {
		t.Fatalf("sent %d reads, want 4", len(sent))
	}
	sameJSON(t, sent[0], `{"cloudId":"`+cloudID+`","name":"listJiraIssueTransitions","inputs":{"cloudId":"`+cloudID+`","issueIdOrKey":"FLD-1"}}`)
	sameJSON(t, sent[1], `{"cloudId":"`+cloudID+`","name":"listJiraIssueAssignableUsers","inputs":{"cloudId":"`+cloudID+`","issueKey":"FLD-1","query":"ana"}}`)
	sameJSON(t, sent[2], `{"cloudId":"`+cloudID+`","name":"listJiraIssueAssignableUsers","inputs":{"cloudId":"`+cloudID+`","issueKey":"FLD-1"}}`)
	sameJSON(t, sent[3], `{"cloudId":"`+cloudID+`","name":"getJiraCurrentUser","inputs":{"cloudId":"`+cloudID+`"}}`)
}

func TestTransitionItems(t *testing.T) {
	var data struct {
		Data struct {
			Transitions []Transition `json:"transitions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(transitionsReply), &data); err != nil {
		t.Fatal(err)
	}
	items := transitionItems(data.Data.Transitions)
	want := []pickItem{
		{id: "21", value: "IN PROGRESS", category: "In Progress", search: "In Progress"},
		{id: "101", value: "IN REVIEW", category: "In Progress", note: "via Send to review", search: "Send to review"},
		{id: "201", value: "BLOCKED", category: "Blocked", search: "Blocked"},
		{id: "31", value: "DONE", category: "Done", search: "Done"},
	}
	if len(items) != len(want) {
		t.Fatalf("items = %+v", items)
	}
	for n := range want {
		if items[n] != want[n] {
			t.Errorf("item %d = %+v, want %+v", n, items[n], want[n])
		}
	}
}

var (
	ctrlW = tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}
	ctrlY = tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}
	enter = tea.KeyPressMsg{Code: tea.KeyEnter}
	down  = tea.KeyPressMsg{Code: tea.KeyDown}
)

func typed(s string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: rune(s[0]), Text: s} }

// press sends the key to the board and returns the picker's command, which the test runs itself.
func press(t *testing.T, m *model, k tea.KeyPressMsg) tea.Cmd {
	t.Helper()
	if m.picker != nil {
		return m.pickerInput(k)
	}
	return m.boardInput(k)
}

func boardOn(j *Jira, issues ...Issue) *model {
	m := &model{jira: j, details: map[string][]string{}, detailOK: map[string]bool{}, inFlight: map[viewKind]bool{}}
	for _, i := range issues {
		m.issues = append(m.issues, issueRow(i))
	}
	return m
}

// searchReply answers the read-back of one ticket, and the search for which of them are mine.
func searchReply(args map[string]any, status, category, assignee string, mine bool) string {
	jql, _ := args["jql"].(string)
	if strings.Contains(jql, "currentUser()") {
		if !mine {
			return `{"issues":[],"isLast":true}`
		}
		return `{"issues":[{"key":"FLD-2","fields":{"summary":"Second"}}],"isLast":true}`
	}
	who := "null"
	if assignee != "" {
		who = `{"displayName":"` + assignee + `"}`
	}
	return `{"issues":[{"key":"FLD-2","fields":{"summary":"Second","status":{"name":"` + status +
		`","statusCategory":{"name":"` + category + `"}},"assignee":` + who + `}}],"isLast":true}`
}

func TestBoardChangesStatus(t *testing.T) {
	j, f := newFakeJira(t, func(tool string, args map[string]any) (string, bool) {
		switch tool {
		case "executeRead":
			return transitionsReply, false
		case "transitionJiraIssue":
			return "ok", false
		case "searchJiraIssuesUsingJql":
			return searchReply(args, "IN REVIEW", "In Progress", "Ana Lopes", false), false
		}
		return "unexpected " + tool, true
	})
	m := boardOn(j, Issue{Key: "FLD-1", Status: "TO DO"}, Issue{Key: "FLD-2", Summary: "Second", Status: "TO DO", Category: "To Do", Assignee: "Ana Lopes"})
	m.cursor = 1
	m.details["FLD-2"] = []string{"# FLD-2: Second", "Status: TO DO"}

	cmd := press(t, m, ctrlW)
	if m.picker == nil || m.picker.kind != pickStatus || m.picker.row.Key != "FLD-2" {
		t.Fatalf("picker = %+v", m.picker)
	}
	m.Update(cmd())
	for _, k := range []tea.KeyPressMsg{typed("r"), typed("e"), typed("v")} {
		press(t, m, k)
	}
	if got := m.choices(); len(got) != 1 || got[0].value != "IN REVIEW" {
		t.Fatalf("choices for %q = %+v", m.picker.query, got)
	}
	cmd = press(t, m, enter)
	if !m.picker.busy {
		t.Fatal("the picker isn't busy while the write runs")
	}
	m.Update(cmd())

	sent := f.sent("transitionJiraIssue")
	if len(sent) != 1 {
		t.Fatalf("sent %d transitions", len(sent))
	}
	sameJSON(t, sent[0], `{"cloudId":"`+cloudID+`","issueIdOrKey":"FLD-2","transitionId":"101"}`)
	if m.picker != nil {
		t.Error("the picker stayed open")
	}
	row := m.issues[1]
	if row.Key != "FLD-2" || row.Status != "IN REVIEW" || row.Category != "In Progress" || row.Issue.Status != "IN REVIEW" {
		t.Errorf("row = %+v", row)
	}
	if _, cached := m.details["FLD-2"]; cached {
		t.Error("the detail still shows the old status")
	}
	if m.statusColor != cGreen || !strings.Contains(m.status, "FLD-2 is IN REVIEW") {
		t.Errorf("status = %q", m.status)
	}
}

func TestBoardChangeErrorStaysOnRow(t *testing.T) {
	j, f := newFakeJira(t, func(tool string, args map[string]any) (string, bool) {
		if tool == "executeRead" {
			return transitionsReply, false
		}
		return "You do not have permission to transition this issue", true
	})
	m := boardOn(j, Issue{Key: "FLD-2", Status: "TO DO", Category: "To Do"})
	m.Update(press(t, m, ctrlW)())
	m.Update(press(t, m, enter)())
	if len(f.sent("searchJiraIssuesUsingJql")) != 0 {
		t.Error("read the ticket back after a failed write")
	}
	if m.issues[0].Status != "TO DO" {
		t.Errorf("row status = %q after a failed write", m.issues[0].Status)
	}
	if m.statusColor != cRed || !strings.Contains(m.status, "FLD-2: changing the status") || !strings.Contains(m.status, "permission") {
		t.Errorf("status = %q", m.status)
	}
	if m.picker == nil || m.picker.busy {
		t.Errorf("picker = %+v, want it open to try again", m.picker)
	}
}

func TestBoardChangesAssignee(t *testing.T) {
	var queries []string
	j, f := newFakeJira(t, func(tool string, args map[string]any) (string, bool) {
		switch tool {
		case "executeRead":
			if args["name"] == "getJiraCurrentUser" {
				return meReply, false
			}
			in, _ := args["inputs"].(map[string]any)
			q, _ := in["query"].(string)
			queries = append(queries, q)
			return usersReply, false
		case "editJiraIssue":
			return `{"key":"FLD-2"}`, false
		case "searchJiraIssuesUsingJql":
			return searchReply(args, "TO DO", "To Do", "Ricardo Ferreira", true), false
		}
		return "unexpected " + tool, true
	})
	m := boardOn(j, Issue{Key: "FLD-2", Summary: "Second", Status: "TO DO", Category: "To Do", Assignee: "Ana Lopes"})

	m.Update(press(t, m, ctrlY)())
	got := m.choices()
	// Me first, then unassigned, then the rest without me twice and without inactive accounts.
	if len(got) != 3 || !got[0].me || got[0].id != "acc-me" || got[1].id != "" || got[1].note != "unassigned" || got[2].id != "acc-ana" {
		t.Fatalf("choices = %+v", got)
	}

	// Typing asks Jira once it pauses; the search's result only counts while it is the latest.
	press(t, m, typed("a"))
	tick := press(t, m, typed("n"))
	if tick == nil {
		t.Fatal("typing doesn't search")
	}
	if cmd, _ := m.pickerUpdate(pickSearchMsg{seq: m.picker.seq - 1}); cmd != nil {
		t.Error("an older keystroke's search ran")
	}
	cmd, _ := m.pickerUpdate(pickSearchMsg{seq: m.picker.seq})
	m.Update(cmd())
	if queries[len(queries)-1] != "an" {
		t.Errorf("searched %q, want the query", queries)
	}
	if got := m.choices(); len(got) != 1 || got[0].id != "acc-ana" {
		t.Errorf("choices for %q = %+v", m.picker.query, got)
	}

	// Back to an empty query: me is one key away.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m.Update(press(t, m, enter)())
	sent := f.sent("editJiraIssue")
	if len(sent) != 1 {
		t.Fatalf("sent %d edits", len(sent))
	}
	sameJSON(t, sent[0], `{"cloudId":"`+cloudID+`","issueIdOrKey":"FLD-2","fields":{"assignee":{"accountId":"acc-me"}}}`)
	row := m.issues[0]
	if row.Assignee != "Ricardo Ferreira" || !row.isMine() {
		t.Errorf("row = %+v, mine %v", row, row.isMine())
	}
	if !strings.Contains(m.status, "FLD-2 is assigned to Ricardo Ferreira") {
		t.Errorf("status = %q", m.status)
	}

	// Unassigning: the second choice.
	m.Update(press(t, m, ctrlY)())
	press(t, m, down)
	m.Update(press(t, m, enter)())
	sent = f.sent("editJiraIssue")
	sameJSON(t, sent[len(sent)-1], `{"cloudId":"`+cloudID+`","issueIdOrKey":"FLD-2","fields":{"assignee":null}}`)
}

func TestPickerIsForTickets(t *testing.T) {
	m := &model{view: prs, prs: []Row{prRow(Pr{Key: "repo#1"})}}
	if cmd := press(t, m, ctrlW); cmd != nil || m.picker != nil {
		t.Error("the status picker opened on a PR")
	}
}
