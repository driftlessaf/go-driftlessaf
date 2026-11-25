# note.Store: passing notes between driftlessAF agents

Package `note` is the note-passing medium for multi-agent driftlessAF runs.
Agents in a run don't share memory, so they hand work to each other by
persisting it: one step `Put`s a note, and a later step `Get`s it back. The store
is both the channel and the durable record of what each agent produced.

The package docs (`doc.go` here and in `gcsstore/`) are the authority on the
contract. This page is a guide to using it.

## Identity

A note is addressed by four coordinates:

| Coordinate | Meaning | Example |
|---|---|---|
| `Key` | The workqueue key the run is about | `harden:widget` |
| `Run` | The attempt for that key, 1-based, shared across chained queues | `1` |
| `Name` | The step that produced it, qualified by stage | `harden/fix_plan` |
| `Author` | The model, synthesizer or human; `""` for a shared note | `claude` |

`note.Ref(key, run, name, author)` is the note's primary key: a SHA-256 over
length-prefixed coordinates. Any agent can compute a peer's `Ref` without a
lookup, so a run passes refs and the store holds the bodies.

## The interface

```go
type Store interface {
	Put(ctx context.Context, n Note, body io.Reader) error
	Get(ctx context.Context, key string, run int, name, author string) (Note, io.ReadCloser, error)
	Delete(ctx context.Context, key string, run int, name, author string) error
	List(ctx context.Context, f Filter) (Page, error)
}
```

- `Put` overwrites. Every backend calls `Note.Validate` first, so an invalid
  note is never stored.
- `Get` and `Delete` return `note.ErrNotExist` for a missing note. Close the
  reader `Get` returns.
- `List` returns coordinates only, never bodies, in bounded pages. A page can
  be short, or even empty, while `Page.Cursor` is set: only an empty cursor ends
  a listing.

Empty `Filter` fields match anything. `Run` and `Author` are pointers, so
`Author: &shared` with `shared := ""` selects shared notes only.

## Backends

| Backend | Use |
|---|---|
| `note.NewMem()` | Tests and single-process runs |
| `gcsstore.New(root, blobs)` | Durable storage over `store/blob`, in production a GCS bucket |

Every backend runs the `notetest.RunConformance` suite, so the in-memory store
behaves like the durable one.

`gcsstore` lays each note out as `<root>key_<esc>/run_<N>/name_<esc>/author_<esc>`,
so one prefix scan gathers a step's notes across authors. It buffers bodies up
to `DefaultMaxNoteSize` (64 MiB); `WithMaxNoteSize` changes the ceiling.

```go
client, err := storage.NewClient(ctx)
if err != nil {
	return err
}
notes, err := gcsstore.New("orgs/example/", gcs.New(client.Bucket("my-notes-bucket")))
if err != nil {
	return err
}

run := 1
if err := notes.Put(ctx, note.Note{Key: "harden:widget", Run: run, Name: "harden/fix_plan", Author: "claude"},
	strings.NewReader("plan A")); err != nil {
	return err
}

f := note.Filter{Key: "harden:widget", Run: &run, Name: "harden/fix_plan"}
for {
	page, err := notes.List(ctx, f)
	if err != nil {
		return err
	}
	for _, n := range page.Notes {
		_, body, err := notes.Get(ctx, n.Key, n.Run, n.Name, n.Author)
		if err != nil {
			return err
		}
		// Read body here.
		body.Close()
	}
	if page.Cursor == "" {
		break
	}
	f.Cursor = page.Cursor
}
```

## Isolation

Isolation is a capability the caller holds, never a coordinate. Because any
agent can compute any `Ref`, a coordinate can't authorize anything.

- **Per tenant:** the `root` passed to `gcsstore.New`. Build one store per tenant
  and pin it at construction.
- **Per attempt:** `note.NewScoped(note.Scope{Key: k, Run: r}, store)` returns a
  handle that fails with `note.ErrOutOfScope` outside that key and run. A scope
  with `Run: 0` admits every run of the key, the broader grant an orchestrator
  uses for retry or synthesis. Nesting can narrow a scope but never widen it.

## Retention

The store has no retention policy. Configure automatic expiration with a bucket
lifecycle rule. Remove specific notes with `List` followed by `Delete`. There is no
delete-by-filter: a sweep is a series of single deletes, so one that fails part
way leaves the notes it didn't reach still listable.
