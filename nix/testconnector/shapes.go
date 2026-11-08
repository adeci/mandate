package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/adeci/mandate/store"
)

// shapes are the fixtures a config's shape picks. Each stores its chats as
// the connector of that name does (convert.go, mailbox.go, ingest.go), with
// the cases that only real accounts showed before: the same channel on two
// servers, a mailbox of many threads, nested folders, names of 120+
// characters with and without spaces, emoji, "~" and duplicate names.
var shapes = map[string]func(now time.Time) ([]store.Container, []store.Message){
	"":         basic,
	"discord":  discord,
	"fastmail": fastmail,
	"signal":   signal,
}

// basic is the VM test's: one message from a month ago beyond a week's
// horizon, and a login code.
func basic(now time.Time) ([]store.Container, []store.Message) {
	sam := store.Participant{ID: "+15550003", Name: "~Sam"}
	old := now.AddDate(0, -1, 0)
	return []store.Container{
		{ID: "fam", Kind: "group", Name: "Family", IsGroup: true},
		{ID: "club", Kind: "group", Name: "Book club", IsGroup: true, Icon: "club-1"},
		{ID: "sam", Kind: "dm", Name: "~Sam", Participants: []store.Participant{sam}, Icon: "sam-1"},
	}, []store.Message{
		{ID: "fam/1", ContainerID: "fam", SenderID: "+15550001", SenderName: "Mum", SentAt: old, Body: "dinner at 7?"},
		{ID: "fam/2", ContainerID: "fam", SenderID: "+15550001", SenderName: "Mum", SentAt: now.Add(-2 * time.Minute), Body: "fresh family"},
		{ID: "fam/3", ContainerID: "fam", SenderID: "+15550002", SenderName: "Dad", SentAt: now.Add(-time.Minute), Body: "Your login code: 482913"},
		{ID: "club/1", ContainerID: "club", SenderID: "+15550004", SenderName: "Ann", SentAt: now.Add(-time.Minute), Body: "fresh club"},
		{ID: "sam/1", ContainerID: "sam", SenderID: sam.ID, SenderName: sam.Name, SentAt: now.Add(-time.Minute), Body: "fresh secret"},
	}
}

// discord has guilds "main" and "other", each with #general and #random, a
// thread under main's #general, and enough of main's #general to page.
// Guilds are guild:ID, as the connector stores them, and main's #general
// has main's own ID, as the first channel of an old server does.
func discord(now time.Time) ([]store.Container, []store.Message) {
	kai := store.Participant{ID: "u-kai", Name: "~kai"}
	cs := []store.Container{
		{ID: "guild:main", Kind: "guild", Name: "main", Icon: "main-1"},
		{ID: "guild:other", Kind: "guild", Name: "other", Icon: "other-1"},
		{ID: "main", Kind: "channel", ParentID: "guild:main", Name: "#general"},
		{ID: "main-random", Kind: "channel", ParentID: "guild:main", Name: "#random"},
		{ID: "other-general", Kind: "channel", ParentID: "guild:other", Name: "#general"},
		{ID: "other-random", Kind: "channel", ParentID: "guild:other", Name: "#random"},
		{ID: "main-rollout", Kind: "thread", ParentID: "main", Name: "rollout plan"},
		{ID: "dm-kai", Kind: "dm", Name: "~kai", Participants: []store.Participant{kai}},
	}
	var ms []store.Message
	for i := range 80 {
		ms = append(ms, store.Message{
			ID: fmt.Sprintf("main-general/%d", i), ContainerID: "main", SenderID: "u-ada", SenderName: "ada",
			SentAt: now.Add(-time.Duration(80-i) * time.Minute), Body: fmt.Sprintf("main general %d", i),
		})
	}
	for _, c := range []string{"main-random", "other-general", "other-random", "main-rollout"} {
		ms = append(ms, store.Message{
			ID: c + "/1", ContainerID: c, SenderID: "u-bo", SenderName: "bo",
			SentAt: now.Add(-time.Minute), Body: strings.ReplaceAll(c, "-", " "),
		})
	}
	ms = append(ms, store.Message{ID: "dm-kai/1", ContainerID: "dm-kai", SenderID: kai.ID, SenderName: kai.Name, SentAt: now.Add(-time.Minute), Body: "see you there"})
	return cs, ms
}

// fastmail has Inbox, Archive, Trash and Projects / 2026, and 30 threads
// filed in them, named after their subjects.
func fastmail(now time.Time) ([]store.Container, []store.Message) {
	box := func(id string) string { return "mailbox:" + id }
	cs := []store.Container{
		{ID: box("inbox"), Kind: "mailbox", Name: "Inbox"},
		{ID: box("archive"), Kind: "mailbox", Name: "Archive"},
		{ID: box("trash"), Kind: "mailbox", Name: "Trash"},
		{ID: box("projects"), Kind: "mailbox", Name: "Projects"},
		{ID: box("2026"), Kind: "mailbox", ParentID: box("projects"), Name: "2026"},
	}
	subjects := []string{
		"Quarterly planning notes for the infrastructure migration, including the database cutover schedule, rollback plan and on-call rota",
		"Invoice_2026-11_ACME-Corporation_Professional-Services_Statement-of-Work_Amendment-No.3_Final_Signed_Countersigned_Archive.pdf",
		"Your order has shipped",
		"Dinner on Saturday?",
		"🎉 Party photos",
	}
	for i := len(subjects); i < 30; i++ {
		subjects = append(subjects, fmt.Sprintf("Weekly update %d", i))
	}
	boxes := []string{"inbox", "inbox", "2026", "archive", "trash"}
	var ms []store.Message
	for i, s := range subjects {
		id := fmt.Sprintf("T%02d", i)
		cs = append(cs, store.Container{ID: id, Kind: "thread", ParentID: box(boxes[i%len(boxes)]), Name: s})
		for j := range 2 {
			ms = append(ms, store.Message{
				ID: fmt.Sprintf("%s/%d", id, j), ContainerID: id, SenderID: "ana@example.com", SenderName: "Ana Lima",
				SentAt: now.Add(-time.Duration(60*i+j) * time.Minute), Body: fmt.Sprintf("%s, message %d", s, j),
			})
		}
	}
	return cs, ms
}

// signal has contacts with emoji, "~" profile names and the same name
// twice, Note to Self, and groups with emoji and a 120+ character name.
func signal(now time.Time) ([]store.Container, []store.Message) {
	dm := func(id, name string) store.Container {
		return store.Container{ID: id, Kind: "dm", Name: name, Participants: []store.Participant{{ID: id, Name: name}}}
	}
	cs := []store.Container{
		dm("aci-self", "Note to Self"),
		dm("aci-mum", "Mum ❤️"),
		dm("aci-sam", "~Sam"),
		dm("aci-alex-1", "Alex"),
		dm("aci-alex-2", "Alex"),
		{ID: "grp-hike", Kind: "group", IsGroup: true, Name: "🏔️ Hiking crew 🥾"},
		{ID: "grp-long", Kind: "group", IsGroup: true, Name: "Neighbourhood association for the streets between the river, the old railway line and the park, including the allotments"},
	}
	var ms []store.Message
	for i, c := range cs {
		ms = append(ms, store.Message{
			ID: c.ID + "/1", ContainerID: c.ID, SenderID: c.ID, SenderName: c.Name,
			SentAt: now.Add(-time.Duration(i+1) * time.Minute), Body: "hello from " + c.Name,
		})
	}
	return cs, ms
}
