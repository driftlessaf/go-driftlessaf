/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package linearreconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AddLabelToIssue adds a label to a Linear issue by their UUIDs.
func (c *Client) AddLabelToIssue(ctx context.Context, issueID, labelID string) error {
	const mutation = `mutation($id: String!, $labelId: String!) {
		issueAddLabel(id: $id, labelId: $labelId) {
			success
		}
	}`

	var result struct {
		IssueAddLabel struct {
			Success bool `json:"success"`
		} `json:"issueAddLabel"`
	}
	err := c.graphql(ctx, mutation, map[string]any{
		"id":      issueID,
		"labelId": labelID,
	}, &result)
	if err != nil {
		return err
	}
	if !result.IssueAddLabel.Success {
		return fmt.Errorf("adding label %s to issue %s: API returned success=false", labelID, issueID)
	}
	return nil
}

// ErrLabelNotFound is returned by FindLabelID when no workspace label with
// the requested name exists. Distinct from a transient GraphQL error — use
// errors.Is to decide whether to surface "label needs creating" guidance
// rather than retrying.
var ErrLabelNotFound = errors.New("label not found")

// FindLabelID searches for a label by name and returns its UUID. Returns
// ErrLabelNotFound (wrappable) when no matching label exists in the workspace.
func (c *Client) FindLabelID(ctx context.Context, name string) (string, error) {
	const query = `query($filter: IssueLabelFilter) {
		issueLabels(filter: $filter) {
			nodes { id name }
		}
	}`

	var result struct {
		IssueLabels struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"issueLabels"`
	}
	err := c.graphql(ctx, query, map[string]any{
		"filter": map[string]any{
			"name": map[string]any{"eq": name},
		},
	}, &result)
	if err != nil {
		return "", fmt.Errorf("searching for label %q: %w", name, err)
	}

	for _, l := range result.IssueLabels.Nodes {
		if strings.EqualFold(l.Name, name) {
			return l.ID, nil
		}
	}
	return "", fmt.Errorf("label %q: %w", name, ErrLabelNotFound)
}

// CreateIssueInput is the input for CreateIssue.
type CreateIssueInput struct {
	TeamID      string
	Title       string
	Description string
	ProjectID   string // optional
	MilestoneID string // optional (projectMilestoneId)
	ParentID    string // optional
	Priority    int    // 0=None, 1=Urgent, 2=High, 3=Normal, 4=Low
	// StateID is the workflow state UUID (e.g. team's "Todo" state).
	// When empty, Linear applies the team's default — typically Triage,
	// which is rarely what bot-created issues want. Resolve the UUID
	// once per team via FindStateIDByType and reuse across a batch to
	// avoid creating issues in Triage and then issuing N follow-up
	// updates.
	StateID string

	// AssigneeID is the Linear user UUID to assign the new issue to.
	// Optional — when empty, Linear leaves the issue unassigned (or applies
	// any team-default workflow rules). Callers typically pass the parent
	// issue's Creator.ID so generated children inherit the human shepherd.
	AssigneeID string
}

// CreateIssueResult is what CreateIssue returns.
type CreateIssueResult struct {
	ID         string
	Identifier string
	URL        string
}

// CreateIssue creates a Linear issue and returns its UUID, public identifier
// (e.g. "DEV-123"), and URL.
func (c *Client) CreateIssue(ctx context.Context, input CreateIssueInput) (CreateIssueResult, error) {
	const mutation = `mutation($input: IssueCreateInput!) {
		issueCreate(input: $input) {
			success
			issue { id identifier url }
		}
	}`
	vars := map[string]any{
		"teamId":      input.TeamID,
		"title":       input.Title,
		"description": input.Description,
	}
	if input.ProjectID != "" {
		vars["projectId"] = input.ProjectID
	}
	if input.MilestoneID != "" {
		vars["projectMilestoneId"] = input.MilestoneID
	}
	if input.ParentID != "" {
		vars["parentId"] = input.ParentID
	}
	if input.Priority > 0 {
		vars["priority"] = input.Priority
	}
	if input.StateID != "" {
		vars["stateId"] = input.StateID
	}
	if input.AssigneeID != "" {
		vars["assigneeId"] = input.AssigneeID
	}

	var result struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				URL        string `json:"url"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{"input": vars}, &result); err != nil {
		return CreateIssueResult{}, fmt.Errorf("create issue: %w", err)
	}
	if !result.IssueCreate.Success {
		return CreateIssueResult{}, fmt.Errorf("create issue: API returned success=false")
	}
	return CreateIssueResult{
		ID:         result.IssueCreate.Issue.ID,
		Identifier: result.IssueCreate.Issue.Identifier,
		URL:        result.IssueCreate.Issue.URL,
	}, nil
}

// CreateProjectInput is the input for CreateProject.
type CreateProjectInput struct {
	Name        string
	Description string
	TeamIDs     []string
	State       string // e.g. "backlog", "planned", "started"
}

// CreateProjectResult is what CreateProject returns.
type CreateProjectResult struct {
	ID  string
	URL string
}

// CreateProject creates a Linear project and returns its UUID and URL.
func (c *Client) CreateProject(ctx context.Context, input CreateProjectInput) (CreateProjectResult, error) {
	const mutation = `mutation($input: ProjectCreateInput!) {
		projectCreate(input: $input) {
			success
			project { id url }
		}
	}`
	vars := map[string]any{
		"name":        input.Name,
		"description": input.Description,
		"teamIds":     input.TeamIDs,
	}
	if input.State != "" {
		vars["state"] = input.State
	}

	var result struct {
		ProjectCreate struct {
			Success bool `json:"success"`
			Project struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"project"`
		} `json:"projectCreate"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{"input": vars}, &result); err != nil {
		return CreateProjectResult{}, fmt.Errorf("create project: %w", err)
	}
	if !result.ProjectCreate.Success {
		return CreateProjectResult{}, fmt.Errorf("create project: API returned success=false")
	}
	return CreateProjectResult{
		ID:  result.ProjectCreate.Project.ID,
		URL: result.ProjectCreate.Project.URL,
	}, nil
}

// FindActiveProjectIDByName returns the UUID of an existing Linear project in
// the given team whose name matches `name` and whose state is not `canceled`
// or `completed`. Returns "" if no matching active project exists.
//
// Callers use this to dedup before CreateProject — the Linear API has no
// uniqueness constraint on project names within a team, so without an
// explicit pre-check a retry, a stale state attachment, or a re-enqueued
// reconcile event will silently produce duplicate projects.
//
// Match is case-insensitive and exact (no fuzzy matching) so callers must
// pass the canonical project name they intend to create.
func (c *Client) FindActiveProjectIDByName(ctx context.Context, teamID, name string) (string, error) {
	if teamID == "" {
		return "", fmt.Errorf("teamID is required")
	}
	if name == "" {
		return "", fmt.Errorf("name is required")
	}

	// Paginate: projects(first: 250) is Linear's max page size, and scaffolding
	// actively grows a team's project count — once it exceeds one page, an
	// existing active project on a later page becomes invisible and the dedup
	// silently fails, scaffolding a duplicate. Walk every page via the cursor.
	// (A server-side projects(filter: { name: { eqIgnoreCase } }) would remove
	// the limit in one query; left as a follow-up pending confirmation of the
	// team.projects filter schema.)
	const query = `query($teamId: String!, $after: String) {
		team(id: $teamId) {
			projects(first: 250, after: $after) {
				nodes {
					id
					name
					state
				}
				pageInfo {
					hasNextPage
					endCursor
				}
			}
		}
	}`

	wantName := strings.ToLower(name)
	var after *string
	for {
		var result struct {
			Team struct {
				Projects struct {
					Nodes []struct {
						ID    string `json:"id"`
						Name  string `json:"name"`
						State string `json:"state"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"projects"`
			} `json:"team"`
		}
		vars := map[string]any{"teamId": teamID}
		if after != nil {
			vars["after"] = *after
		}
		if err := c.graphql(ctx, query, vars, &result); err != nil {
			return "", fmt.Errorf("find active project by name: %w", err)
		}

		for _, p := range result.Team.Projects.Nodes {
			if strings.ToLower(p.Name) != wantName {
				continue
			}
			// Linear's project state field is a free-form string set per
			// workspace; canceled/completed are the standard terminal values.
			// Anything else (backlog, planned, started, paused) counts as
			// "still occupying the namespace" for dedup purposes.
			if p.State == "canceled" || p.State == "completed" {
				continue
			}
			return p.ID, nil
		}

		if !result.Team.Projects.PageInfo.HasNextPage {
			return "", nil
		}
		cursor := result.Team.Projects.PageInfo.EndCursor
		after = &cursor
	}
}

// CreateMilestoneInput is the input for CreateMilestone.
type CreateMilestoneInput struct {
	ProjectID   string
	Name        string
	Description string
	SortOrder   float64
}

// CreateMilestone creates a Linear project milestone and returns its UUID.
func (c *Client) CreateMilestone(ctx context.Context, input CreateMilestoneInput) (string, error) {
	const mutation = `mutation($input: ProjectMilestoneCreateInput!) {
		projectMilestoneCreate(input: $input) {
			success
			projectMilestone { id }
		}
	}`
	vars := map[string]any{
		"projectId":   input.ProjectID,
		"name":        input.Name,
		"description": input.Description,
	}
	if input.SortOrder != 0 {
		vars["sortOrder"] = input.SortOrder
	}

	var result struct {
		ProjectMilestoneCreate struct {
			Success          bool `json:"success"`
			ProjectMilestone struct {
				ID string `json:"id"`
			} `json:"projectMilestone"`
		} `json:"projectMilestoneCreate"`
	}
	if err := c.graphql(ctx, mutation, map[string]any{"input": vars}, &result); err != nil {
		return "", fmt.Errorf("create milestone: %w", err)
	}
	if !result.ProjectMilestoneCreate.Success {
		return "", fmt.Errorf("create milestone: API returned success=false")
	}
	return result.ProjectMilestoneCreate.ProjectMilestone.ID, nil
}

// CreateDocumentInput is the input for CreateDocument.
// Exactly one of IssueID or ProjectID must be set.
type CreateDocumentInput struct {
	Title     string
	Content   string
	IssueID   string
	ProjectID string
}

// CreateDocumentResult is what CreateDocument returns.
type CreateDocumentResult struct {
	ID   string
	Slug string
	URL  string
}

// CreateDocument creates a new Linear document parented to either an issue or
// a project. Exactly one of IssueID/ProjectID must be set on the input.
func (c *Client) CreateDocument(ctx context.Context, input CreateDocumentInput) (CreateDocumentResult, error) {
	if (input.IssueID == "") == (input.ProjectID == "") {
		return CreateDocumentResult{}, fmt.Errorf("CreateDocument: exactly one of IssueID or ProjectID must be set")
	}

	const mutation = `mutation($input: DocumentCreateInput!) {
		documentCreate(input: $input) {
			success
			document {
				id
				slugId
				url
			}
		}
	}`

	vars := map[string]any{
		"title":   input.Title,
		"content": input.Content,
	}
	if input.IssueID != "" {
		vars["issueId"] = input.IssueID
	}
	if input.ProjectID != "" {
		vars["projectId"] = input.ProjectID
	}

	var result struct {
		DocumentCreate struct {
			Success  bool `json:"success"`
			Document *struct {
				ID     string `json:"id"`
				SlugID string `json:"slugId"`
				URL    string `json:"url"`
			} `json:"document"`
		} `json:"documentCreate"`
	}

	if err := c.graphql(ctx, mutation, map[string]any{"input": vars}, &result); err != nil {
		return CreateDocumentResult{}, fmt.Errorf("create document: %w", err)
	}
	if !result.DocumentCreate.Success || result.DocumentCreate.Document == nil {
		return CreateDocumentResult{}, fmt.Errorf("create document: success=false")
	}
	return CreateDocumentResult{
		ID:   result.DocumentCreate.Document.ID,
		Slug: result.DocumentCreate.Document.SlugID,
		URL:  result.DocumentCreate.Document.URL,
	}, nil
}

// UpdateDocumentInput is the input for UpdateDocument. All fields are optional —
// only the non-zero fields are sent to Linear. To reparent a document under a
// different project, set ProjectID.
type UpdateDocumentInput struct {
	Title     string
	Content   string
	ProjectID string
}

// UpdateDocument updates an existing Linear document. Used today only to
// reparent a document under a project after scaffolding.
func (c *Client) UpdateDocument(ctx context.Context, idOrSlug string, input UpdateDocumentInput) error {
	const mutation = `mutation($id: String!, $input: DocumentUpdateInput!) {
		documentUpdate(id: $id, input: $input) {
			success
		}
	}`

	vars := map[string]any{}
	if input.Title != "" {
		vars["title"] = input.Title
	}
	if input.Content != "" {
		vars["content"] = input.Content
	}
	if input.ProjectID != "" {
		vars["projectId"] = input.ProjectID
	}

	var result struct {
		DocumentUpdate struct {
			Success bool `json:"success"`
		} `json:"documentUpdate"`
	}

	if err := c.graphql(ctx, mutation, map[string]any{"id": idOrSlug, "input": vars}, &result); err != nil {
		return fmt.Errorf("update document: %w", err)
	}
	if !result.DocumentUpdate.Success {
		return fmt.Errorf("update document: success=false")
	}
	return nil
}

// LinkAttachment creates a URL-link attachment on an issue. Use this to attach
// a Linear document URL (or any other URL) to an issue so it appears in the
// issue's Attachments list. Distinct from UploadFileAttachment, which uploads
// file content.
func (c *Client) LinkAttachment(ctx context.Context, issueID, url, title string) error {
	const mutation = `mutation($input: AttachmentCreateInput!) {
		attachmentCreate(input: $input) {
			success
		}
	}`

	vars := map[string]any{
		"issueId": issueID,
		"url":     url,
		"title":   title,
	}

	var result struct {
		AttachmentCreate struct {
			Success bool `json:"success"`
		} `json:"attachmentCreate"`
	}

	if err := c.graphql(ctx, mutation, map[string]any{"input": vars}, &result); err != nil {
		return fmt.Errorf("create attachment: %w", err)
	}
	if !result.AttachmentCreate.Success {
		return fmt.Errorf("create attachment: success=false")
	}
	return nil
}
