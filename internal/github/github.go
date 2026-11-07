// Package github reads and writes the PM and code repositories (issues, comments, sub-issues, Git
// Data) and the Project through the go-gh REST and GraphQL clients. Field and option node ids are
// resolved by name at startup.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/oh-my-omp/pm-council/internal/config"
)

// Client talks to GitHub with one token.
type Client struct {
	rest *api.RESTClient
	gql  *api.GraphQLClient
}

// New builds REST and GraphQL clients authenticated with token.
func New(token string) (*Client, error) {
	opts := api.ClientOptions{AuthToken: token}
	rest, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, fmt.Errorf("github REST client: %w", err)
	}
	gql, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, fmt.Errorf("github GraphQL client: %w", err)
	}
	return &Client{rest: rest, gql: gql}, nil
}

// Select is a resolved single-select field: its node id and option ids by GitHub option name.
type Select struct {
	ID      string
	Options map[string]string
}

// Project is the resolved Project: node ids of the project and of each configured field.
type Project struct {
	ID       string
	Status   Select
	Kind     Select
	Agenda   string
	Proposer Select
}

const fieldsSelection = `id fields(first:50){nodes{
  ... on ProjectV2SingleSelectField{id name options{id name}}
  ... on ProjectV2Field{id name dataType}}}`

type projectFields struct {
	ID     string `json:"id"`
	Fields struct {
		Nodes []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			DataType string `json:"dataType"`
			Options  []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"options"`
		} `json:"nodes"`
	} `json:"fields"`
}

// ResolveProject looks up the Project by owner login and number (organization first, then user)
// and resolves every field and option named in p. A missing name is an error.
func (c *Client) ResolveProject(ctx context.Context, ref config.ProjectRef, p config.ProjectFields) (Project, error) {
	vars := map[string]any{"login": ref.Owner, "number": ref.Number}
	var org struct {
		Organization struct {
			ProjectV2 *projectFields `json:"projectV2"`
		} `json:"organization"`
	}
	err := c.gql.DoWithContext(ctx, `query($login:String!,$number:Int!){organization(login:$login){projectV2(number:$number){`+fieldsSelection+`}}}`, vars, &org)
	pf := org.Organization.ProjectV2
	var gqlErr *api.GraphQLError
	if errors.As(err, &gqlErr) && gqlErr.Match("NOT_FOUND", "organization") {
		var user struct {
			User struct {
				ProjectV2 *projectFields `json:"projectV2"`
			} `json:"user"`
		}
		err = c.gql.DoWithContext(ctx, `query($login:String!,$number:Int!){user(login:$login){projectV2(number:$number){`+fieldsSelection+`}}}`, vars, &user)
		pf = user.User.ProjectV2
	}
	if err != nil {
		return Project{}, fmt.Errorf("project %s #%d: %w", ref.Owner, ref.Number, err)
	}
	if pf == nil {
		return Project{}, fmt.Errorf("project %s #%d: not found", ref.Owner, ref.Number)
	}

	var errs []error
	type fieldInfo struct {
		id, dataType string
		options      map[string]string
		single       bool
	}
	fields := map[string]fieldInfo{}
	for _, n := range pf.Fields.Nodes {
		if n.ID == "" {
			continue
		}
		fi := fieldInfo{id: n.ID, dataType: n.DataType, options: map[string]string{}, single: n.Options != nil}
		for _, o := range n.Options {
			fi.options[o.Name] = o.ID
		}
		fields[n.Name] = fi
	}
	single := func(key, name string, options []config.Named) Select {
		fi, ok := fields[name]
		if !ok || !fi.single {
			errs = append(errs, fmt.Errorf("shared/project.yaml: %s.name: Project has no single-select field %q", key, name))
			return Select{}
		}
		for _, o := range options {
			if _, ok := fi.options[o.Name]; !ok {
				errs = append(errs, fmt.Errorf("shared/project.yaml: %s.options.%s: field %q has no option %q", key, o.Key, name, o.Name))
			}
		}
		return Select{ID: fi.id, Options: fi.options}
	}
	out := Project{
		ID:       pf.ID,
		Status:   single("fields.status", p.Status.Name, p.Status.Options.List()),
		Kind:     single("fields.kind", p.Kind.Name, p.Kind.Options.List()),
		Proposer: single("fields.proposer", p.Proposer.Name, p.Proposer.Options.List()),
	}
	if fi, ok := fields[p.Agenda.Name]; !ok || fi.dataType != "TEXT" {
		errs = append(errs, fmt.Errorf("shared/project.yaml: fields.agenda.name: Project has no text field %q", p.Agenda.Name))
	} else {
		out.Agenda = fi.id
	}
	if err := errors.Join(errs...); err != nil {
		return Project{}, err
	}
	return out, nil
}

// Item is one Project item that is an issue: the item id, the issue, whether it is closed and its
// field values by GitHub field name (single-select option name or text).
type Item struct {
	ID     string
	Repo   string
	Number int
	Closed bool
	Fields map[string]string
}

const fieldValuesSelection = `fieldValues(first:20){nodes{
  ... on ProjectV2ItemFieldSingleSelectValue{name field{... on ProjectV2SingleSelectField{name}}}
  ... on ProjectV2ItemFieldTextValue{text field{... on ProjectV2Field{name}}}}}`

type fieldValues struct {
	Nodes []struct {
		Name  *string `json:"name"`
		Text  *string `json:"text"`
		Field struct {
			Name string `json:"name"`
		} `json:"field"`
	} `json:"nodes"`
}

func (fv fieldValues) toMap() map[string]string {
	m := map[string]string{}
	for _, n := range fv.Nodes {
		switch {
		case n.Field.Name == "":
		case n.Name != nil:
			m[n.Field.Name] = *n.Name
		case n.Text != nil:
			m[n.Field.Name] = *n.Text
		}
	}
	return m
}

// ProjectItems lists every Project item whose content is an issue.
func (c *Client) ProjectItems(ctx context.Context, projectID string) ([]Item, error) {
	const q = `query($id:ID!,$c:String){node(id:$id){... on ProjectV2{items(first:100,after:$c){
  nodes{id content{... on Issue{number closed repository{nameWithOwner}}} ` + fieldValuesSelection + `}
  pageInfo{hasNextPage endCursor}}}}}`
	var items []Item
	var cursor *string
	for {
		var res struct {
			Node struct {
				Items struct {
					Nodes []struct {
						ID      string `json:"id"`
						Content *struct {
							Number     int  `json:"number"`
							Closed     bool `json:"closed"`
							Repository struct {
								NameWithOwner string `json:"nameWithOwner"`
							} `json:"repository"`
						} `json:"content"`
						FieldValues fieldValues `json:"fieldValues"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"items"`
			} `json:"node"`
		}
		if err := c.gql.DoWithContext(ctx, q, map[string]any{"id": projectID, "c": cursor}, &res); err != nil {
			return nil, fmt.Errorf("project items: %w", err)
		}
		for _, n := range res.Node.Items.Nodes {
			if n.Content == nil || n.Content.Number == 0 {
				continue // draft issue, pull request or redacted content
			}
			items = append(items, Item{ID: n.ID, Repo: n.Content.Repository.NameWithOwner, Number: n.Content.Number, Closed: n.Content.Closed, Fields: n.FieldValues.toMap()})
		}
		if !res.Node.Items.PageInfo.HasNextPage {
			return items, nil
		}
		cursor = new(res.Node.Items.PageInfo.EndCursor)
	}
}

// IssueItem returns the issue's item in the Project; ok is false when the issue does not exist
// or is not in the Project.
func (c *Client) IssueItem(ctx context.Context, repo string, number int, projectID string) (item Item, ok bool, err error) {
	owner, name, _ := strings.Cut(repo, "/")
	const q = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issue(number:$number){
  number closed repository{nameWithOwner} projectItems(first:50){nodes{id project{id} ` + fieldValuesSelection + `}}}}}`
	var res struct {
		Repository struct {
			Issue *struct {
				Number     int  `json:"number"`
				Closed     bool `json:"closed"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
				ProjectItems struct {
					Nodes []struct {
						ID      string `json:"id"`
						Project struct {
							ID string `json:"id"`
						} `json:"project"`
						FieldValues fieldValues `json:"fieldValues"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	err = c.gql.DoWithContext(ctx, q, map[string]any{"owner": owner, "name": name, "number": number}, &res)
	var gqlErr *api.GraphQLError
	if errors.As(err, &gqlErr) && gqlErr.Match("NOT_FOUND", "repository.issue") {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("issue %s#%d: %w", repo, number, err)
	}
	is := res.Repository.Issue
	if is == nil {
		return Item{}, false, nil
	}
	for _, n := range is.ProjectItems.Nodes {
		if n.Project.ID == projectID {
			return Item{ID: n.ID, Repo: is.Repository.NameWithOwner, Number: is.Number, Closed: is.Closed, Fields: n.FieldValues.toMap()}, true, nil
		}
	}
	return Item{}, false, nil
}

// Comment is an issue comment's body and creation time.
type Comment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// IssueComments returns the issue's comments, oldest first.
func (c *Client) IssueComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	cs, err := pages[Comment](ctx, c.rest, fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, number))
	if err != nil {
		return nil, fmt.Errorf("comments of %s#%d: %w", repo, number, err)
	}
	return cs, nil
}

// Comments returns the bodies of the issue's comments, oldest first.
func (c *Client) Comments(ctx context.Context, repo string, number int) ([]string, error) {
	cs, err := c.IssueComments(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	bodies := make([]string, len(cs))
	for i, c := range cs {
		bodies[i] = c.Body
	}
	return bodies, nil
}

// IssueRef names an issue by repository and number.
type IssueRef struct {
	Repo   string
	Number int
}

// BlockedBy returns the issues the issue is blocked by.
func (c *Client) BlockedBy(ctx context.Context, repo string, number int) ([]IssueRef, error) {
	type issue struct {
		Number        int    `json:"number"`
		RepositoryURL string `json:"repository_url"`
	}
	is, err := pages[issue](ctx, c.rest, fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by?per_page=100", repo, number))
	if err != nil {
		return nil, fmt.Errorf("blocked_by of %s#%d: %w", repo, number, err)
	}
	refs := make([]IssueRef, 0, len(is))
	for _, i := range is {
		_, r, ok := strings.Cut(i.RepositoryURL, "/repos/")
		if !ok {
			return nil, fmt.Errorf("blocked_by of %s#%d: unexpected repository_url %q", repo, number, i.RepositoryURL)
		}
		refs = append(refs, IssueRef{Repo: r, Number: i.Number})
	}
	return refs, nil
}

// Issue is an issue's node id, title, body and whether it is closed.
type Issue struct {
	NodeID string
	Title  string
	Body   string
	Closed bool
}

// Issue reads an issue.
func (c *Client) Issue(ctx context.Context, repo string, number int) (Issue, error) {
	var is struct {
		NodeID string  `json:"node_id"`
		Title  string  `json:"title"`
		Body   *string `json:"body"`
		State  string  `json:"state"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/issues/%d", repo, number), nil, &is); err != nil {
		return Issue{}, fmt.Errorf("issue %s#%d: %w", repo, number, err)
	}
	out := Issue{NodeID: is.NodeID, Title: is.Title, Closed: is.State == "closed"}
	if is.Body != nil {
		out.Body = *is.Body
	}
	return out, nil
}

// Comment returns the body of the issue comment id in repo.
func (c *Client) Comment(ctx context.Context, repo string, id int64) (string, error) {
	var cm struct {
		Body string `json:"body"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/issues/comments/%d", repo, id), nil, &cm); err != nil {
		return "", fmt.Errorf("comment %d of %s: %w", id, repo, err)
	}
	return cm.Body, nil
}

// PostComment posts a comment on the issue and returns its id.
func (c *Client) PostComment(ctx context.Context, repo string, number int, body string) (int64, error) {
	b, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return 0, err
	}
	var cm struct {
		ID int64 `json:"id"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", repo, number), bytes.NewReader(b), &cm); err != nil {
		return 0, fmt.Errorf("comment on %s#%d: %w", repo, number, err)
	}
	return cm.ID, nil
}

// SetText sets a text field of a Project item.
func (c *Client) SetText(ctx context.Context, projectID, itemID, fieldID, text string) error {
	return c.setField(ctx, projectID, itemID, fieldID, map[string]any{"text": text})
}

// SetOption sets a single-select field of a Project item.
func (c *Client) SetOption(ctx context.Context, projectID, itemID, fieldID, optionID string) error {
	return c.setField(ctx, projectID, itemID, fieldID, map[string]any{"singleSelectOptionId": optionID})
}

func (c *Client) setField(ctx context.Context, projectID, itemID, fieldID string, value map[string]any) error {
	const m = `mutation($project:ID!,$item:ID!,$field:ID!,$value:ProjectV2FieldValue!){
  updateProjectV2ItemFieldValue(input:{projectId:$project,itemId:$item,fieldId:$field,value:$value}){projectV2Item{id}}}`
	var res json.RawMessage
	vars := map[string]any{"project": projectID, "item": itemID, "field": fieldID, "value": value}
	if err := c.gql.DoWithContext(ctx, m, vars, &res); err != nil {
		return fmt.Errorf("update item %s field %s: %w", itemID, fieldID, err)
	}
	return nil
}

// EditIssueBody replaces the issue's body.
func (c *Client) EditIssueBody(ctx context.Context, repo string, number int, body string) error {
	return c.patchIssue(ctx, repo, number, map[string]string{"body": body})
}

// CloseReason is the state_reason of a closed issue.
type CloseReason string

const (
	Completed  CloseReason = "completed"
	NotPlanned CloseReason = "not_planned"
)

// CloseIssue closes the issue with reason.
func (c *Client) CloseIssue(ctx context.Context, repo string, number int, reason CloseReason) error {
	return c.patchIssue(ctx, repo, number, map[string]string{"state": "closed", "state_reason": string(reason)})
}

func (c *Client) patchIssue(ctx context.Context, repo string, number int, fields map[string]string) error {
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var res json.RawMessage
	if err := c.rest.DoWithContext(ctx, http.MethodPatch, fmt.Sprintf("repos/%s/issues/%d", repo, number), bytes.NewReader(b), &res); err != nil {
		return fmt.Errorf("update %s#%d: %w", repo, number, err)
	}
	return nil
}

// SubIssue is a sub-issue of an issue.
type SubIssue struct {
	Repo   string
	Number int
	NodeID string
	Title  string
	Body   string
}

// SubIssues lists the issue's sub-issues.
func (c *Client) SubIssues(ctx context.Context, repo string, number int) ([]SubIssue, error) {
	type issue struct {
		Number        int     `json:"number"`
		NodeID        string  `json:"node_id"`
		Title         string  `json:"title"`
		Body          *string `json:"body"`
		RepositoryURL string  `json:"repository_url"`
	}
	is, err := pages[issue](ctx, c.rest, fmt.Sprintf("repos/%s/issues/%d/sub_issues?per_page=100", repo, number))
	if err != nil {
		return nil, fmt.Errorf("sub-issues of %s#%d: %w", repo, number, err)
	}
	out := make([]SubIssue, 0, len(is))
	for _, i := range is {
		_, r, ok := strings.Cut(i.RepositoryURL, "/repos/")
		if !ok {
			return nil, fmt.Errorf("sub-issues of %s#%d: unexpected repository_url %q", repo, number, i.RepositoryURL)
		}
		s := SubIssue{Repo: r, Number: i.Number, NodeID: i.NodeID, Title: i.Title}
		if i.Body != nil {
			s.Body = *i.Body
		}
		out = append(out, s)
	}
	return out, nil
}

// NewIssue is an issue to create as a sub-issue of the issue with node id ParentID, as an item of
// the Project with node id ProjectID.
type NewIssue struct {
	Repo, Title, Body   string
	ParentID, ProjectID string
}

// CreateIssue creates n in one GraphQL createIssue mutation, so that the issue never exists
// without its parent: a retry finds it among the parent's sub-issues.
func (c *Client) CreateIssue(ctx context.Context, n NewIssue) (SubIssue, error) {
	owner, name, _ := strings.Cut(n.Repo, "/")
	var repo struct {
		Repository struct {
			ID string `json:"id"`
		} `json:"repository"`
	}
	if err := c.gql.DoWithContext(ctx, `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){id}}`,
		map[string]any{"owner": owner, "name": name}, &repo); err != nil {
		return SubIssue{}, fmt.Errorf("repository %s: %w", n.Repo, err)
	}
	const m = `mutation($repo:ID!,$title:String!,$body:String!,$parent:ID!,$projects:[ID!]){
  createIssue(input:{repositoryId:$repo,title:$title,body:$body,parentIssueId:$parent,projectV2Ids:$projects}){issue{number id}}}`
	var res struct {
		CreateIssue struct {
			Issue struct {
				Number int    `json:"number"`
				ID     string `json:"id"`
			} `json:"issue"`
		} `json:"createIssue"`
	}
	vars := map[string]any{"repo": repo.Repository.ID, "title": n.Title, "body": n.Body, "parent": n.ParentID, "projects": []string{n.ProjectID}}
	if err := c.gql.DoWithContext(ctx, m, vars, &res); err != nil {
		return SubIssue{}, fmt.Errorf("create issue in %s: %w", n.Repo, err)
	}
	is := res.CreateIssue.Issue
	return SubIssue{Repo: n.Repo, Number: is.Number, NodeID: is.ID, Title: n.Title, Body: n.Body}, nil
}

// AddProjectItem adds the issue with node id contentID to the Project and returns the item id;
// an issue already in the Project returns its item.
func (c *Client) AddProjectItem(ctx context.Context, projectID, contentID string) (string, error) {
	const m = `mutation($project:ID!,$content:ID!){addProjectV2ItemById(input:{projectId:$project,contentId:$content}){item{id}}}`
	var res struct {
		Add struct {
			Item struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	if err := c.gql.DoWithContext(ctx, m, map[string]any{"project": projectID, "content": contentID}, &res); err != nil {
		return "", fmt.Errorf("add %s to the Project: %w", contentID, err)
	}
	return res.Add.Item.ID, nil
}

// DefaultBranch returns the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, "repos/"+repo, nil, &r); err != nil {
		return "", fmt.Errorf("repository %s: %w", repo, err)
	}
	return r.DefaultBranch, nil
}

// FileExists reports whether path exists in repo at branch.
func (c *Client) FileExists(ctx context.Context, repo, branch, path string) (bool, error) {
	var res json.RawMessage
	err := c.rest.DoWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, escapePath(path), url.QueryEscape(branch)), nil, &res)
	var he *api.HTTPError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &he) && he.StatusCode == http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("%s:%s at %s: %w", repo, path, branch, err)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// CommitFiles writes files (path → content) to branch of repo as one commit on its head through
// the Git Data API: blobs, a tree on the head's tree, a commit, then a non-forced ref update.
func (c *Client) CommitFiles(ctx context.Context, repo, branch, message string, files map[string]string) (string, error) {
	post := func(path string, in, out any) error {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		return c.rest.DoWithContext(ctx, http.MethodPost, fmt.Sprintf("repos/%s/%s", repo, path), bytes.NewReader(b), out)
	}
	type sha struct {
		SHA string `json:"sha"`
	}
	var ref struct {
		Object sha `json:"object"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/git/ref/heads/%s", repo, escapePath(branch)), nil, &ref); err != nil {
		return "", fmt.Errorf("head of %s %s: %w", repo, branch, err)
	}
	var head struct {
		Tree sha `json:"tree"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/git/commits/%s", repo, ref.Object.SHA), nil, &head); err != nil {
		return "", fmt.Errorf("commit %s of %s: %w", ref.Object.SHA, repo, err)
	}
	type entry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	entries := make([]entry, 0, len(paths))
	for _, p := range paths {
		var blob sha
		if err := post("git/blobs", map[string]string{"content": files[p], "encoding": "utf-8"}, &blob); err != nil {
			return "", fmt.Errorf("blob %s in %s: %w", p, repo, err)
		}
		entries = append(entries, entry{Path: p, Mode: "100644", Type: "blob", SHA: blob.SHA})
	}
	var tree sha
	if err := post("git/trees", map[string]any{"base_tree": head.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", fmt.Errorf("tree in %s: %w", repo, err)
	}
	var commit sha
	if err := post("git/commits", map[string]any{"message": message, "tree": tree.SHA, "parents": []string{ref.Object.SHA}}, &commit); err != nil {
		return "", fmt.Errorf("commit in %s: %w", repo, err)
	}
	b, err := json.Marshal(map[string]any{"sha": commit.SHA, "force": false})
	if err != nil {
		return "", err
	}
	var res json.RawMessage
	if err := c.rest.DoWithContext(ctx, http.MethodPatch, fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, escapePath(branch)), bytes.NewReader(b), &res); err != nil {
		return "", fmt.Errorf("update %s %s to %s: %w", repo, branch, commit.SHA, err)
	}
	return commit.SHA, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// pages GETs path and every following page named by the Link header.
func pages[T any](ctx context.Context, rest *api.RESTClient, path string) ([]T, error) {
	var all []T
	for path != "" {
		resp, err := rest.RequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var page []T
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		path = ""
		if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			path = m[1]
		}
	}
	return all, nil
}
