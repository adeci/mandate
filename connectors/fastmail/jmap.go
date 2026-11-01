package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultSessionURL = "https://api.fastmail.com/jmap/session"

	capCore       = "urn:ietf:params:jmap:core"
	capMail       = "urn:ietf:params:jmap:mail"
	capSubmission = "urn:ietf:params:jmap:submission"

	maxResponse = 64 << 20
)

var errUnauthorized = errors.New("the API token was refused")

// statusError is an HTTP answer other than 200. JMAP rejects a whole
// request before running any of its methods, so a 4xx means nothing ran.
type statusError struct {
	code   int
	detail string
}

func (e *statusError) Error() string {
	if e.detail != "" {
		return fmt.Sprintf("Fastmail answered %d: %s", e.code, e.detail)
	}
	return fmt.Sprintf("Fastmail answered %d", e.code)
}

// methodError is a JMAP method that failed as a whole.
type methodError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func (e *methodError) Error() string {
	if e.Description != "" {
		return e.Type + ": " + e.Description
	}
	return e.Type
}

type session struct {
	APIURL          string            `json:"apiUrl"`
	Username        string            `json:"username"`
	PrimaryAccounts map[string]string `json:"primaryAccounts"`
	Accounts        map[string]struct {
		IsReadOnly bool `json:"isReadOnly"`
	} `json:"accounts"`
}

// client speaks JMAP with one API token.
type client struct {
	http  *http.Client
	token string
}

func (c *client) do(req *http.Request) ([]byte, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errUnauthorized
	case resp.StatusCode != http.StatusOK:
		var problem struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &problem)
		return nil, &statusError{code: resp.StatusCode, detail: strings.TrimSpace(problem.Type + " " + problem.Detail)}
	}
	return body, nil
}

func (c *client) session(ctx context.Context, url string) (session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return session{}, err
	}
	body, err := c.do(req)
	if err != nil {
		return session{}, err
	}
	var s session
	if err := json.Unmarshal(body, &s); err != nil {
		return session{}, fmt.Errorf("decode session: %w", err)
	}
	if s.APIURL == "" {
		return session{}, errors.New("the session names no API URL")
	}
	return s, nil
}

type invocation struct {
	name string
	args any
	id   string
}

func (i invocation) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{i.name, i.args, i.id})
}

type response struct {
	name string
	args json.RawMessage
	id   string
}

// call runs calls in one request.
func (c *client) call(ctx context.Context, api string, calls ...invocation) ([]response, error) {
	b, err := json.Marshal(map[string]any{
		"using":       []string{capCore, capMail, capSubmission},
		"methodCalls": calls,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var raw struct {
		MethodResponses [][3]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	resps := make([]response, len(raw.MethodResponses))
	for i, r := range raw.MethodResponses {
		if err := json.Unmarshal(r[0], &resps[i].name); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if err := json.Unmarshal(r[2], &resps[i].id); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		resps[i].args = r[1]
	}
	return resps, nil
}

// result decodes the response named name to the call with id.
func result[T any](resps []response, name, id string) (T, error) {
	var v T
	for _, r := range resps {
		if r.id != id {
			continue
		}
		switch r.name {
		case "error":
			var e methodError
			if err := json.Unmarshal(r.args, &e); err != nil {
				return v, fmt.Errorf("decode %s error: %w", name, err)
			}
			return v, &e
		case name:
			if err := json.Unmarshal(r.args, &v); err != nil {
				return v, fmt.Errorf("decode %s: %w", name, err)
			}
			return v, nil
		}
	}
	return v, fmt.Errorf("no %s in the response", name)
}

type getResponse[T any] struct {
	State    string   `json:"state"`
	List     []T      `json:"list"`
	NotFound []string `json:"notFound"`
}

type queryResponse struct {
	IDs []string `json:"ids"`
}

type changesResponse struct {
	NewState       string   `json:"newState"`
	HasMoreChanges bool     `json:"hasMoreChanges"`
	Created        []string `json:"created"`
	Updated        []string `json:"updated"`
	Destroyed      []string `json:"destroyed"`
}

type setError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func (e setError) String() string {
	if e.Description != "" {
		return e.Type + ": " + e.Description
	}
	return e.Type
}

type setResponse struct {
	Created map[string]struct {
		ID string `json:"id"`
	} `json:"created"`
	NotCreated map[string]setError `json:"notCreated"`
}

type mailbox struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

type identity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type bodyPart struct {
	PartID string `json:"partId,omitempty"`
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
}

type bodyValue struct {
	Value       string `json:"value"`
	IsTruncated bool   `json:"isTruncated,omitempty"`
}

type email struct {
	ID          string               `json:"id"`
	ThreadID    string               `json:"threadId"`
	MailboxIDs  map[string]bool      `json:"mailboxIds"`
	Keywords    map[string]bool      `json:"keywords"`
	From        []address            `json:"from"`
	To          []address            `json:"to"`
	Cc          []address            `json:"cc"`
	ReplyTo     []address            `json:"replyTo"`
	Subject     string               `json:"subject"`
	ReceivedAt  time.Time            `json:"receivedAt"`
	MessageID   []string             `json:"messageId"`
	InReplyTo   []string             `json:"inReplyTo"`
	References  []string             `json:"references"`
	TextBody    []bodyPart           `json:"textBody"`
	BodyValues  map[string]bodyValue `json:"bodyValues"`
	Attachments []bodyPart           `json:"attachments"`
}

var emailProperties = []string{
	"id", "threadId", "mailboxIds", "keywords", "from", "to", "cc", "replyTo", "subject", "receivedAt",
	"messageId", "inReplyTo", "references", "textBody", "bodyValues", "attachments",
}

// utcDate formats t as a JMAP UTCDate.
func utcDate(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
