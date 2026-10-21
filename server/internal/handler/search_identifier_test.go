package handler

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestParseQueryNumber(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"MUL-123", 123},
		{"mul-123", 123},
		{"  123  ", 123},
		{"V2-12", 12},
		{"  v2-12  ", 12},
		{"A1-7", 7},
		{"a1b2-42", 42},
		{"12-3", 0},
		{"2FAS-1", 0},
		{"V2-0", 0},
		{"V2--12", 0},
		{"V2-12x", 0},
		{"V_2-12", 0},
		{"MUL-", 0},
		{"search", 0},
		{"0", 0},
		{"", 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, ok := parseQueryNumber(tc.query)
			if got != tc.want || ok != (tc.want > 0) {
				t.Fatalf("parseQueryNumber(%q) = (%d, %t), want (%d, %t)", tc.query, got, ok, tc.want, tc.want > 0)
			}
		})
	}
}

func TestParseQueryIdentifier(t *testing.T) {
	for _, tc := range []struct {
		query     string
		prefix    string
		number    int
		hasNumber bool
	}{
		{"GOAL-5", "GOAL", 5, true},
		{"goal-5", "GOAL", 5, true},
		{"  Mul-123 ", "MUL", 123, true},
		{"V2-12", "V2", 12, true},
		{"123", "", 123, true},
		{"GOAL", "GOAL", 0, false},
		{"goal-", "GOAL", 0, false},
		{"A", "A", 0, false},
		{"abcdefghij", "ABCDEFGHIJ", 0, false},
		{"abcdefghijk", "", 0, false},
		{"hello world", "", 0, false},
		{"12-3", "", 0, false},
		{"V2-0", "", 0, false},
		{"0", "", 0, false},
		{"", "", 0, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			prefix, number, ok := parseQueryIdentifier(tc.query)
			if prefix != tc.prefix || number != tc.number || ok != tc.hasNumber {
				t.Fatalf("parseQueryIdentifier(%q) = (%q, %d, %t), want (%q, %d, %t)",
					tc.query, prefix, number, ok, tc.prefix, tc.number, tc.hasNumber)
			}
		})
	}
}

func TestBuildSearchQueryWithPrefix(t *testing.T) {
	terminal := []string{"done", "cancelled"}

	query, args := buildSearchQueryWithPrefix("GOAL-5", []string{"GOAL-5"}, 5, true, "GOAL", false, terminal)
	if !strings.Contains(query, "i.identifier_prefix =") || !strings.Contains(query, "AS number_exact") {
		t.Fatalf("prefix+number must constrain both columns:\n%s", query)
	}
	if strings.Contains(query, "prefix_match") {
		t.Fatalf("prefix+number must not add the prefix-only flag:\n%s", query)
	}
	foundPrefix, foundNumber := false, false
	for _, a := range args {
		if s, ok := a.(string); ok && s == "GOAL" {
			foundPrefix = true
		}
		if n, ok := a.(int); ok && n == 5 {
			foundNumber = true
		}
	}
	if !foundPrefix || !foundNumber {
		t.Fatalf("args missing prefix/number: %v", args)
	}

	query, _ = buildSearchQueryWithPrefix("GOAL", []string{"GOAL"}, 0, false, "GOAL", false, terminal)
	if !strings.Contains(query, "prefix_match") || strings.Contains(query, "number_exact") {
		t.Fatalf("prefix-only must use prefix_match and no number_exact:\n%s", query)
	}

	// Bare number keeps the legacy any-prefix behaviour.
	query, _ = buildSearchQueryWithPrefix("5", []string{"5"}, 5, true, "", false, terminal)
	if strings.Contains(query, "identifier_prefix =") || strings.Contains(query, "prefix_match") {
		t.Fatalf("bare number must not filter on prefix:\n%s", query)
	}
}

func TestIssueSearch_DigitPrefix(t *testing.T) {
	workspaceID := dbfx.Workspace(t, "Digit prefix search", "digit-search-"+uuid.NewString(), testutil.Cols{"issue_prefix": "V2"})
	dbfx.Member(t, workspaceID, testUserID, "owner")
	targetID := dbfx.Issue(t, "Abandoned plan", testutil.Cols{
		"workspace_id": workspaceID, "number": 12, "status": "cancelled",
	})
	liveID := dbfx.Issue(t, "Notes about V2-12", testutil.Cols{"workspace_id": workspaceID})
	foreignWorkspaceID := dbfx.Workspace(t, "Other search workspace", "other-search-"+uuid.NewString())
	dbfx.Issue(t, "V2-12", testutil.Cols{"workspace_id": foreignWorkspaceID, "number": 12})

	for _, query := range []string{"V2-12", "v2-12", "12"} {
		t.Run(query, func(t *testing.T) {
			// The fallback API must retain the exact cancelled hit ahead of live
			// text matches, even when the result window has only one slot.
			for _, includeClosed := range []bool{true, false} {
				path := fmt.Sprintf("/api/issues/search?q=%s&include_closed=%t&limit=1", url.QueryEscape(query), includeClosed)
				req := newRequest(http.MethodGet, path, nil)
				req.Header.Set("X-Workspace-ID", workspaceID)
				var response struct {
					Issues []SearchIssueResponse `json:"issues"`
				}
				testutil.Call(t, testHandler.SearchIssues, req).Want(http.StatusOK).JSON(&response)
				wantID := liveID
				if includeClosed {
					wantID = targetID
				}
				if len(response.Issues) != 1 || response.Issues[0].ID != wantID {
					t.Fatalf("search include_closed=%t: got %+v, want issue %s", includeClosed, response.Issues, wantID)
				}
			}

			// List search uses the same parser while preserving active filters
			// and the total for the filtered result set.
			for _, status := range []string{"cancelled", "todo"} {
				path := fmt.Sprintf("/api/issues?q=%s&status=%s&limit=1", url.QueryEscape(query), status)
				req := newRequest(http.MethodGet, path, nil)
				req.Header.Set("X-Workspace-ID", workspaceID)
				var response struct {
					Issues []IssueResponse `json:"issues"`
					Total  int64           `json:"total"`
				}
				testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusOK).JSON(&response)
				wantID := liveID
				if status == "cancelled" {
					wantID = targetID
				}
				if response.Total != 1 || len(response.Issues) != 1 || response.Issues[0].ID != wantID {
					t.Fatalf("list status=%s: got %+v, want only issue %s", status, response, wantID)
				}
			}
		})
	}
}

// Numbers repeat across projects, so an identifier query has to match the
// project prefix as well as the number, and a bare prefix has to list that
// project's whole series. This is the multi-letter prefix case ("GOAL").
func TestIssueSearch_ProjectPrefix(t *testing.T) {
	workspaceID := dbfx.Workspace(t, "Prefix search", "prefix-search-"+uuid.NewString(), testutil.Cols{"issue_prefix": "EXP"})
	dbfx.Member(t, workspaceID, testUserID, "owner")
	goal2 := dbfx.Issue(t, "Alpha goal", testutil.Cols{"workspace_id": workspaceID, "number": 2, "identifier_prefix": "GOAL"})
	goal5 := dbfx.Issue(t, "Beta goal", testutil.Cols{"workspace_id": workspaceID, "number": 5, "identifier_prefix": "GOAL"})
	exp5 := dbfx.Issue(t, "Gamma exp", testutil.Cols{"workspace_id": workspaceID, "number": 5, "identifier_prefix": "EXP"})

	search := func(t *testing.T, path string, out any) {
		t.Helper()
		req := newRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Workspace-ID", workspaceID)
		testutil.Call(t, testHandler.SearchIssues, req).Want(http.StatusOK).JSON(out)
	}
	ids := func(t *testing.T, q string) []string {
		t.Helper()
		var response struct {
			Issues []SearchIssueResponse `json:"issues"`
		}
		search(t, fmt.Sprintf("/api/issues/search?q=%s&include_closed=true", url.QueryEscape(q)), &response)
		out := make([]string, 0, len(response.Issues))
		for _, i := range response.Issues {
			out = append(out, i.ID)
		}
		sort.Strings(out)
		return out
	}
	want := func(ids ...string) []string { sort.Strings(ids); return ids }

	for q, expected := range map[string][]string{
		"GOAL-5": want(goal5),
		"goal-5": want(goal5),
		"EXP-5":  want(exp5),
		"5":      want(goal5, exp5),
		"GOAL":   want(goal2, goal5),
		"goal-":  want(goal2, goal5),
		"EXP":    want(exp5),
	} {
		t.Run(q, func(t *testing.T) {
			if got := ids(t, q); fmt.Sprint(got) != fmt.Sprint(expected) {
				t.Fatalf("search %q = %v, want %v", q, got, expected)
			}
			// The table filter shares the parser and must agree.
			var response struct {
				Issues []IssueResponse `json:"issues"`
			}
			req := newRequest(http.MethodGet, "/api/issues?limit=50&q="+url.QueryEscape(q), nil)
			req.Header.Set("X-Workspace-ID", workspaceID)
			testutil.Call(t, testHandler.ListIssues, req).Want(http.StatusOK).JSON(&response)
			got := make([]string, 0, len(response.Issues))
			for _, i := range response.Issues {
				got = append(got, i.ID)
			}
			sort.Strings(got)
			if fmt.Sprint(got) != fmt.Sprint(expected) {
				t.Fatalf("list %q = %v, want %v", q, got, expected)
			}
		})
	}
}
