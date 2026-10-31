package client

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/proposal"
)

// maxPoll matches the gateway's long-poll cap; longer waits take several.
const maxPoll = 900 * time.Second

func (c *client) propose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	note := fs.String("note", "", "why, shown to the owner")
	wait := fs.Duration("wait", 0, "block until the owner decides, up to this long (e.g. 15m)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return usage(proposeUsage, err)
	}
	if len(positional) < 2 {
		return usage(proposeUsage, nil)
	}
	kv, err := keyValues(positional[2:])
	if err != nil {
		return usage(proposeUsage, err)
	}
	return c.submit(ctx, proposal.Request{Connector: positional[0], Op: positional[1], Args: kv, Note: *note}, *wait)
}

// httpRequest reads with GET and HEAD through the gateway's passthrough;
// any other method changes something, so it becomes an `http request`
// proposal.
func (c *client) httpRequest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	data := fs.String("data", "", "request body")
	note := fs.String("note", "", "why, shown to the owner")
	wait := fs.Duration("wait", 0, "block until the owner decides, up to this long (e.g. 15m)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return usage(httpUsage, err)
	}
	if len(positional) != 3 {
		return usage(httpUsage, nil)
	}
	method, service, path := strings.ToUpper(positional[0]), positional[1], positional[2]
	if method != http.MethodGet && method != http.MethodHead {
		kv := map[string]string{"service": service, "method": method, "path": "/" + strings.TrimPrefix(path, "/")}
		if *data != "" {
			kv["body"] = *data
		}
		return c.submit(ctx, proposal.Request{Connector: "http", Op: "request", Args: kv, Note: *note}, *wait)
	}
	if *data != "" || *note != "" || *wait != 0 {
		return usage(httpUsage, fmt.Errorf("--data, --note and --wait are for writes; %s reads directly", method))
	}
	out, err := c.send(ctx, requestTimeout, method, "/v1/http/"+url.PathEscape(service)+"/"+strings.TrimPrefix(path, "/"), nil)
	if err != nil {
		return err
	}
	_, err = c.out.Write(out)
	return err
}

func (c *client) submit(ctx context.Context, req proposal.Request, wait time.Duration) error {
	if wait < 0 {
		return usage(proposeUsage, errors.New("--wait must not be negative"))
	}
	var raw json.RawMessage
	if err := c.post(ctx, "/v1/actions", req, &raw); err != nil {
		return err
	}
	p, err := c.show(raw)
	if err != nil {
		return err
	}
	if wait == 0 {
		if c.table {
			fmt.Fprintf(c.out, "then check with: mandate status %s --wait 15m\n", p.ID)
		}
		return nil
	}
	fmt.Fprintf(c.errOut, "waiting up to %s for the owner to decide; send them %s\n", wait, p.URL)
	return c.await(ctx, p.ID, wait)
}

func (c *client) status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	wait := fs.Duration("wait", 0, "block until the owner decides, up to this long (e.g. 15m)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return usage(statusUsage, err)
	}
	if len(positional) != 1 {
		return usage(statusUsage, nil)
	}
	if *wait < 0 {
		return usage(statusUsage, errors.New("--wait must not be negative"))
	}
	return c.await(ctx, positional[0], *wait)
}

// await long-polls the gateway until the proposal settles or wait runs out,
// then prints it and maps its status to the exit code.
func (c *client) await(ctx context.Context, id string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		path := "/v1/actions/" + url.PathEscape(id)
		poll := min(time.Until(deadline), maxPoll)
		var secs int
		if poll > 0 {
			secs = int(math.Ceil(poll.Seconds()))
			path += "?wait=" + strconv.Itoa(secs)
		}
		raw, err := c.send(ctx, time.Duration(secs)*time.Second+requestTimeout, http.MethodGet, path, nil)
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

// show prints a proposal: the gateway's JSON when piped, a summary on a TTY.
func (c *client) show(raw json.RawMessage) (proposal.Proposal, error) {
	var p proposal.Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if !c.table {
		_, err := c.out.Write(append(raw, '\n'))
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
		fmt.Fprintf(w, "send the owner this link to approve: %s\n", p.URL)
	}
}

// outcome maps a proposal's status to an exit code and tells the agent what
// to do next.
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
			msg += fmt.Sprintf("; make sure the owner has %s, then", p.URL)
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
		return fmt.Errorf("proposal %s may have been sent%s; never retry automatically, tell the owner", p.ID, detail)
	}
	return fmt.Errorf("proposal %s has status %q; tell the owner", p.ID, p.Status)
}
