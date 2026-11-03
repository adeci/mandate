package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const statusUsage = "mandate status ID [--wait 15m]"

type proposal struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result *struct {
		Ref    string `json:"ref"`
		Detail string `json:"detail"`
		Output string `json:"output"`
	} `json:"result"`
}

// submit runs an op. A read answers rows; a write answers 202 with a
// proposal, which is printed and, with wait, awaited.
func (c *cli) submit(ctx context.Context, path string, args map[string]string, note string, wait time.Duration) error {
	body := struct {
		Args map[string]string `json:"args"`
		Note string            `json:"note,omitempty"`
	}{args, note}
	raw, status, err := c.do(ctx, requestTimeout, http.MethodPost, path, body, nil)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return c.table(raw)
	}
	var p proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if wait == 0 {
		c.show(raw, p)
		return outcome(p, 0)
	}
	if !c.json {
		fmt.Fprintf(c.out, "%s pending\n", p.ID)
	}
	fmt.Fprintf(c.errOut, "waiting up to %s for the owner to decide in the mandate app\n", human(wait))
	return c.await(ctx, p.ID, wait)
}

func (c *cli) status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	wait := fs.Duration("wait", defaultWait, "")
	pos, err := c.parse(fs, statusUsage, args, 1, 1)
	if err != nil {
		return err
	}
	if *wait < 0 {
		return usage(statusUsage, errNegativeWait)
	}
	return c.await(ctx, pos[0], *wait)
}

// await long-polls until the proposal settles or wait runs out, then prints
// it and maps its status to the exit code.
func (c *cli) await(ctx context.Context, id string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		q, timeout := waitQuery(deadline)
		var p proposal
		raw, _, err := c.do(ctx, timeout, http.MethodGet, "/agent/v1/proposals/"+url.PathEscape(id)+q, nil, &p)
		if err != nil {
			return err
		}
		if pending(p.Status) && time.Now().Before(deadline) {
			continue
		}
		c.show(raw, p)
		return outcome(p, wait)
	}
}

func pending(s string) bool { return s == "pending" || s == "executing" }

func (c *cli) show(raw []byte, p proposal) {
	if c.json {
		fmt.Fprintln(c.out, strings.TrimSpace(string(raw)))
		return
	}
	line := p.ID + " " + p.Status
	if r := p.Result; r != nil && r.Ref != "" {
		line += " ref=" + sanitize(r.Ref)
	}
	fmt.Fprintln(c.out, line)
	if r := p.Result; r != nil && r.Output != "" {
		fmt.Fprintln(c.out, sanitize(r.Output))
	}
}

func outcome(p proposal, waited time.Duration) error {
	detail := ""
	if p.Result != nil && p.Result.Detail != "" {
		detail = ": " + sanitize(p.Result.Detail)
	}
	switch p.Status {
	case "done":
		return nil
	case "pending", "executing":
		msg := fmt.Sprintf("proposal %s is still %s", p.ID, p.Status)
		if waited > 0 {
			msg += " after " + human(waited)
		}
		if p.Status == "pending" {
			msg += "; make sure the owner knows it waits in the mandate app, then"
		} else {
			msg += "; it is running,"
		}
		msg += fmt.Sprintf(" check again with: mandate status %s", p.ID)
		return &exitError{msg: msg, code: errTryLater}
	case "rejected":
		return fmt.Errorf("proposal %s was rejected by the owner; don't retry unless they ask", p.ID)
	case "expired":
		return fmt.Errorf("proposal %s expired before the owner decided; propose again only if they still want it", p.ID)
	case "failed":
		return fmt.Errorf("proposal %s failed%s; it was not sent, fix the cause before proposing again", p.ID, detail)
	case "unknown":
		return fmt.Errorf("proposal %s may have been sent%s; don't retry, tell the owner", p.ID, detail)
	}
	return fmt.Errorf("proposal %s has status %q; tell the owner", p.ID, p.Status)
}
