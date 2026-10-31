// Package admin holds commands run on the data host over SSH. They read source
// databases directly and are not reachable through the gateway.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	_ "github.com/ncruces/go-sqlite3/driver" // registers "sqlite3"
)

func List(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	senders := fs.Bool("senders", false, "list senders with message counts instead of containers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: mandate list [--senders] <data.db>")
	}
	db, err := sql.Open("sqlite3", "file:"+fs.Arg(0)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()

	query := `SELECT c.id, c.kind, COALESCE(c.parent_id, ''), COALESCE(c.name, ''),
       (SELECT count(*) FROM messages m WHERE m.container_id = c.id)
FROM containers c ORDER BY c.kind, c.name`
	header := "ID\tKIND\tPARENT\tNAME\tMESSAGES"
	if *senders {
		query = `SELECT COALESCE(sender_id, ''), COALESCE(max(sender_name), ''), count(*)
FROM messages GROUP BY sender_id ORDER BY count(*) DESC`
		header = "SENDER_ID\tNAME\tMESSAGES"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, header)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i, v := range vals {
			if i > 0 {
				fmt.Fprint(w, "\t")
			}
			fmt.Fprint(w, v)
		}
		fmt.Fprintln(w)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}
