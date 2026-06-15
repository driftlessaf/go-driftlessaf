/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddLabelToIssue(t *testing.T) {
	var gotVars map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		gotVars = req.Variables

		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueAddLabel": map[string]any{
					"success": true,
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	err := client.AddLabelToIssue(t.Context(), "issue-uuid", "label-uuid")
	if err != nil {
		t.Fatalf("AddLabelToIssue() error: %v", err)
	}

	if gotVars["id"] != "issue-uuid" {
		t.Errorf("issue ID = %v, want issue-uuid", gotVars["id"])
	}
	if gotVars["labelId"] != "label-uuid" {
		t.Errorf("label ID = %v, want label-uuid", gotVars["labelId"])
	}
}

func TestFindLabelID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueLabels": map[string]any{
					"nodes": []map[string]any{
						{"id": "label-1", "name": "bug"},
						{"id": "label-2", "name": "materializer:managed"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)

	id, err := client.FindLabelID(t.Context(), "materializer:managed")
	if err != nil {
		t.Fatalf("FindLabelID() error: %v", err)
	}
	if id != "label-2" {
		t.Errorf("FindLabelID() = %v, want label-2", id)
	}

	_, err = client.FindLabelID(t.Context(), "nonexistent")
	if err == nil {
		t.Error("FindLabelID() expected error for nonexistent label")
	}
}

func TestCreateIssue(t *testing.T) {
	var gotInput map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if input, ok := req.Variables["input"].(map[string]any); ok {
			gotInput = input
		}

		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue": map[string]any{
						"id":         "issue-uuid-123",
						"identifier": "DEV-456",
						"url":        "https://linear.app/test/issue/DEV-456",
					},
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	res, err := client.CreateIssue(t.Context(), CreateIssueInput{
		TeamID:      "team-abc",
		Title:       "Test Issue",
		Description: "Test description",
		ProjectID:   "proj-xyz",
		Priority:    2,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error: %v", err)
	}
	if res.ID != "issue-uuid-123" {
		t.Errorf("CreateIssue() id = %v, want issue-uuid-123", res.ID)
	}
	if res.Identifier != "DEV-456" {
		t.Errorf("CreateIssue() identifier = %v, want DEV-456", res.Identifier)
	}
	if res.URL != "https://linear.app/test/issue/DEV-456" {
		t.Errorf("CreateIssue() url = %v, want https://linear.app/test/issue/DEV-456", res.URL)
	}
	if gotInput["teamId"] != "team-abc" {
		t.Errorf("teamId = %v, want team-abc", gotInput["teamId"])
	}
	if gotInput["title"] != "Test Issue" {
		t.Errorf("title = %v, want Test Issue", gotInput["title"])
	}
	if gotInput["projectId"] != "proj-xyz" {
		t.Errorf("projectId = %v, want proj-xyz", gotInput["projectId"])
	}
}

// TestCreateIssue_AssigneeID pins the assigneeId wire format: the field
// must only appear in the GraphQL input when AssigneeID is non-empty, so
// callers passing "" (no parent author resolved) keep getting Linear's
// default-assignee behavior instead of accidentally writing assigneeId:"".
func TestCreateIssue_AssigneeID(t *testing.T) {
	var gotInput map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		gotInput, _ = req.Variables["input"].(map[string]any)
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue":   map[string]any{"id": "i", "identifier": "DEV-1", "url": "u"},
				},
			},
		})
	}))
	defer srv.Close()
	client := NewClientWithAPIKey("k").WithEndpoint(srv.URL)

	// Empty: no key in the variables map at all.
	_, err := client.CreateIssue(t.Context(), CreateIssueInput{TeamID: "t", Title: "T"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, present := gotInput["assigneeId"]; present {
		t.Errorf("assigneeId present in vars when AssigneeID was empty: %v", gotInput)
	}

	// Non-empty: key present and equal.
	gotInput = nil
	_, err = client.CreateIssue(t.Context(), CreateIssueInput{TeamID: "t", Title: "T", AssigneeID: "user-uuid"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got := gotInput["assigneeId"]; got != "user-uuid" {
		t.Errorf("assigneeId = %v, want user-uuid", got)
	}
}

func TestCreateIssue_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueCreate": map[string]any{
					"success": false,
					"issue":   nil,
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	_, err := client.CreateIssue(t.Context(), CreateIssueInput{
		TeamID: "team-abc",
		Title:  "Test",
	})
	if err == nil {
		t.Fatal("CreateIssue() expected error on success=false")
	}
}

func TestCreateProject(t *testing.T) {
	var gotInput map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if input, ok := req.Variables["input"].(map[string]any); ok {
			gotInput = input
		}

		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"projectCreate": map[string]any{
					"success": true,
					"project": map[string]any{
						"id":  "project-uuid-456",
						"url": "https://linear.app/test/project/abc",
					},
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	res, err := client.CreateProject(t.Context(), CreateProjectInput{
		Name:        "Test Project",
		Description: "A test project",
		TeamIDs:     []string{"team-abc"},
		State:       "planned",
	})
	if err != nil {
		t.Fatalf("CreateProject() error: %v", err)
	}
	if res.ID != "project-uuid-456" {
		t.Errorf("CreateProject() id = %v, want project-uuid-456", res.ID)
	}
	if res.URL != "https://linear.app/test/project/abc" {
		t.Errorf("CreateProject() url = %v, want https://linear.app/test/project/abc", res.URL)
	}
	if gotInput["name"] != "Test Project" {
		t.Errorf("name = %v, want Test Project", gotInput["name"])
	}
	if gotInput["state"] != "planned" {
		t.Errorf("state = %v, want planned", gotInput["state"])
	}
}

func TestCreateProject_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"projectCreate": map[string]any{
					"success": false,
					"project": nil,
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	_, err := client.CreateProject(t.Context(), CreateProjectInput{
		Name:    "Test",
		TeamIDs: []string{"team-abc"},
	})
	if err == nil {
		t.Fatal("CreateProject() expected error on success=false")
	}
}

func TestCreateMilestone(t *testing.T) {
	var gotInput map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if input, ok := req.Variables["input"].(map[string]any); ok {
			gotInput = input
		}

		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"projectMilestoneCreate": map[string]any{
					"success": true,
					"projectMilestone": map[string]any{
						"id": "milestone-uuid-789",
					},
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	id, err := client.CreateMilestone(t.Context(), CreateMilestoneInput{
		ProjectID:   "project-uuid-456",
		Name:        "Milestone 1",
		Description: "First milestone",
		SortOrder:   1.0,
	})
	if err != nil {
		t.Fatalf("CreateMilestone() error: %v", err)
	}
	if id != "milestone-uuid-789" {
		t.Errorf("CreateMilestone() id = %v, want milestone-uuid-789", id)
	}
	if gotInput["projectId"] != "project-uuid-456" {
		t.Errorf("projectId = %v, want project-uuid-456", gotInput["projectId"])
	}
	if gotInput["name"] != "Milestone 1" {
		t.Errorf("name = %v, want Milestone 1", gotInput["name"])
	}
}

func TestCreateMilestone_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"projectMilestoneCreate": map[string]any{
					"success":          false,
					"projectMilestone": nil,
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	_, err := client.CreateMilestone(t.Context(), CreateMilestoneInput{
		ProjectID: "project-uuid-456",
		Name:      "Milestone 1",
	})
	if err == nil {
		t.Fatal("CreateMilestone() expected error on success=false")
	}
}

func TestCreateDocument_Issue(t *testing.T) {
	var gotInput map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if input, ok := req.Variables["input"].(map[string]any); ok {
			gotInput = input
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"documentCreate": map[string]any{
					"success": true,
					"document": map[string]any{
						"id":     "doc-uuid-1",
						"slugId": "abc123",
						"url":    "https://linear.app/test/document/abc123",
					},
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	res, err := client.CreateDocument(t.Context(), CreateDocumentInput{
		Title:   "Design: foo",
		Content: "# heading\n\nbody",
		IssueID: "issue-1",
	})
	if err != nil {
		t.Fatalf("CreateDocument() error: %v", err)
	}
	if res.ID != "doc-uuid-1" || res.Slug != "abc123" {
		t.Errorf("CreateDocument() = %+v", res)
	}
	if res.URL != "https://linear.app/test/document/abc123" {
		t.Errorf("URL = %v", res.URL)
	}
	if gotInput["title"] != "Design: foo" {
		t.Errorf("title = %v, want Design: foo", gotInput["title"])
	}
	if gotInput["issueId"] != "issue-1" {
		t.Errorf("issueId = %v, want issue-1", gotInput["issueId"])
	}
	if _, has := gotInput["projectId"]; has {
		t.Errorf("projectId unexpectedly present in input")
	}
}

func TestCreateDocument_Project(t *testing.T) {
	var gotInput map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if input, ok := req.Variables["input"].(map[string]any); ok {
			gotInput = input
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"documentCreate": map[string]any{
					"success":  true,
					"document": map[string]any{"id": "d", "slugId": "s", "url": "u"},
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	_, err := client.CreateDocument(t.Context(), CreateDocumentInput{
		Title:     "x",
		Content:   "y",
		ProjectID: "project-1",
	})
	if err != nil {
		t.Fatalf("CreateDocument() error: %v", err)
	}
	if gotInput["projectId"] != "project-1" {
		t.Errorf("projectId = %v, want project-1", gotInput["projectId"])
	}
	if _, has := gotInput["issueId"]; has {
		t.Errorf("issueId unexpectedly present in input")
	}
}

func TestCreateDocument_BothOrNeither(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("graphql endpoint should not be hit when input fails pre-flight validation")
	}))
	defer srv.Close()
	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)

	if _, err := client.CreateDocument(t.Context(), CreateDocumentInput{Title: "x", Content: "y"}); err == nil {
		t.Error("CreateDocument() with neither parent expected error, got nil")
	}
	if _, err := client.CreateDocument(t.Context(), CreateDocumentInput{
		Title: "x", Content: "y", IssueID: "i", ProjectID: "p",
	}); err == nil {
		t.Error("CreateDocument() with both parents expected error, got nil")
	}
}

func TestCreateDocument_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"documentCreate": map[string]any{
					"success":  false,
					"document": nil,
				},
			},
		})
	}))
	defer srv.Close()
	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	if _, err := client.CreateDocument(t.Context(), CreateDocumentInput{
		Title: "x", Content: "y", IssueID: "i",
	}); err == nil {
		t.Fatal("CreateDocument() expected error on success=false")
	}
}

func TestUpdateDocument_Reparent(t *testing.T) {
	var gotID string
	var gotInput map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if v, ok := req.Variables["id"].(string); ok {
			gotID = v
		}
		if input, ok := req.Variables["input"].(map[string]any); ok {
			gotInput = input
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"documentUpdate": map[string]any{
					"success": true,
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	err := client.UpdateDocument(t.Context(), "abc123", UpdateDocumentInput{
		ProjectID: "project-9",
	})
	if err != nil {
		t.Fatalf("UpdateDocument() error: %v", err)
	}
	if gotID != "abc123" {
		t.Errorf("id = %v, want abc123", gotID)
	}
	if gotInput["projectId"] != "project-9" {
		t.Errorf("projectId = %v, want project-9", gotInput["projectId"])
	}
}

func TestUpdateDocument_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"documentUpdate": map[string]any{
					"success": false,
				},
			},
		})
	}))
	defer srv.Close()
	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	if err := client.UpdateDocument(t.Context(), "x", UpdateDocumentInput{ProjectID: "p"}); err == nil {
		t.Fatal("UpdateDocument() expected error on success=false")
	}
}

func TestLinkAttachment(t *testing.T) {
	var gotInput map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if input, ok := req.Variables["input"].(map[string]any); ok {
			gotInput = input
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"attachmentCreate": map[string]any{
					"success": true,
				},
			},
		})
	}))
	defer srv.Close()

	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	err := client.LinkAttachment(t.Context(), "issue-1", "https://linear.app/test/document/abc123", "design_doc")
	if err != nil {
		t.Fatalf("LinkAttachment() error: %v", err)
	}
	if gotInput["issueId"] != "issue-1" {
		t.Errorf("issueId = %v, want issue-1", gotInput["issueId"])
	}
	if gotInput["url"] != "https://linear.app/test/document/abc123" {
		t.Errorf("url = %v", gotInput["url"])
	}
	if gotInput["title"] != "design_doc" {
		t.Errorf("title = %v, want design_doc", gotInput["title"])
	}
}

func TestLinkAttachment_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"attachmentCreate": map[string]any{"success": false},
			},
		})
	}))
	defer srv.Close()
	client := NewClientWithAPIKey("test-key").WithEndpoint(srv.URL)
	if err := client.LinkAttachment(t.Context(), "i", "https://linear.app/test/document/x", "t"); err == nil {
		t.Fatal("LinkAttachment() expected error on success=false")
	}
}
