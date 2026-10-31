package client

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/proposal"
)

// maxPoll matches the gateway's long-poll cap; longer waits take several.
const maxPoll = 900 * time.Second

type opRequest struct {
	Args map[string]string `json:"args"`
	Note string            `json:"note,omitempty"`
}

// submit runs an op. A read answers rows; a write answers 202 with a
// proposal, which is printed and, with wait, awaited.
func (c *client) submit(ctx context.Context, path string, args map[string]string, note string, wait time.Duration) error {
	status, _, raw, err := c.do(ctx, requestTimeout, http.MethodPost, path, opRequest{Args: args, Note: note})
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return c.table(raw)
	}
	p, err := c.show(raw)
	if err != nil {
		return err
	}
	if wait == 0 {
		fmt.Fprintf(c.errOut, "tell the owner it is waiting, then: mandate status %s --wait 15m\n", p.ID)
		return nil
	}
	fmt.Fprintf(c.errOut, "waiting up to %s for the owner to decide\n", wait)
	return c.await(ctx, p.ID, wait)
}

func (c *client) status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	wait := fs.Duration("wait", 0, "")
	pos, err := c.parse(fs, statusUsage, args, 1, 1)
	if err != nil {
		return err
	}
	if *wait < 0 {
		return usage(statusUsage, errNegativeWait)
	}
	return c.await(ctx, pos[0], *wait)
}

// await long-polls the gateway until the proposal settles or wait runs out,
// then prints it and maps its status to the exit code.
func (c *client) await(ctx context.Context, id string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		path := "/v1/proposals/" + url.PathEscape(id)
		poll := min(time.Until(deadline), maxPoll)
		var secs int
		if poll > 0 {
			secs = int(math.Ceil(poll.Seconds()))
			path += "?wait=" + strconv.Itoa(secs)
		}
		_, _, raw, err := c.do(ctx, time.Duration(secs)*time.Second+requestTimeout, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var p proposal.Proposal
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if pending(p.Status) && time.Now().Before(deadline) {
			continue
		}
		if _, err := c.show(raw); err != nil {
			return err
		}
		return outcome(p, wait)
	}
}

func pending(s proposal.Status) bool {
	return s == proposal.Pending || s == proposal.Executing
}

func (c *client) show(raw []byte) (proposal.Proposal, error) {
	var p proposal.Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if c.json {
		_, err := c.out.Write(append(bytes.TrimRight(raw, "\n"), '\n'))
		return p, err
	}
	printProposal(c.out, p)
	return p, nil
}

func printProposal(w io.Writer, p proposal.Proposal) {
	fmt.Fprintf(w, "%s  %s  %s %s\n", p.ID, p.Status, p.Connector, p.Op)
	for _, f := range p.Display {
		fmt.Fprintf(w, "  %s: %s\n", f.Label, f.Value)
	}
	if r := p.Result; r != nil {
		fmt.Fprintf(w, "result: %s", r.Status)
		if r.Ref != "" {
			fmt.Fprintf(w, " ref=%s", r.Ref)
		}
		if r.Detail != "" {
			fmt.Fprintf(w, " (%s)", r.Detail)
		}
		fmt.Fprintln(w)
		if r.Output != "" {
			fmt.Fprintln(w, r.Output)
		}
	}
	if pending(p.Status) && p.URL != "" {
		fmt.Fprintf(w, "approve at: %s\n", p.URL)
	}
}

func outcome(p proposal.Proposal, waited time.Duration) error {
	detail := ""
	if p.Result != nil && p.Result.Detail != "" {
		detail = ": " + p.Result.Detail
	}
	switch p.Status {
	case proposal.Done:
		return nil
	case proposal.Pending, proposal.Executing:
		msg := fmt.Sprintf("proposal %s is still %s", p.ID, p.Status)
		if waited > 0 {
			msg += " after " + waited.String()
		}
		if p.Status == proposal.Pending {
			msg += "; make sure the owner knows it is waiting, then"
		} else {
			msg += "; it is running,"
		}
		msg += fmt.Sprintf(" check again with: mandate status %s --wait 15m", p.ID)
		return &exitError{msg: msg, code: exitcode.ErrTryLater}
	case proposal.Rejected:
		return fmt.Errorf("proposal %s was rejected by the owner; don't retry unless they ask", p.ID)
	case proposal.Expired:
		return fmt.Errorf("proposal %s expired before the owner decided; propose again only if they still want it", p.ID)
	case proposal.Failed:
		return fmt.Errorf("proposal %s failed%s; it did not go through, fix the cause before proposing again", p.ID, detail)
	case proposal.Unknown:
		return fmt.Errorf("proposal %s may have been sent%s; don't retry, tell the owner", p.ID, detail)
	}
	return fmt.Errorf("proposal %s has status %q; tell the owner", p.ID, p.Status)
}
