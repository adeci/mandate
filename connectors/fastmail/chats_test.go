package main

import (
	"path/filepath"
	"testing"

	"github.com/adeci/mandate/store/storetest"
)

// The chats a first sync stores, kept in testdata/chats.json for the
// gateway's tests of agents' terms: every mailbox, a nested folder, and
// threads filed in Inbox, Archive and the folder.
func TestChatsFixture(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seed(f.jm)
	f.jm.set(func(j *fakeJMAP) {
		j.mailboxes = append(j.mailboxes, mailbox{ID: "mb-rec", Name: "Receipts", ParentID: "mb-inbox"})
	})
	f.jm.add(textEmail("e9", "t9", address{Name: "Shop", Email: "orders@shop.example"}, []address{owner}, "Your order has shipped", "x", now, "mb-rec"))
	f.link()
	storetest.Check(t, f.c.st.DB(), filepath.Join("testdata", "chats.json"))
}
