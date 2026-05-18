package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cursorQuery builds a properly URL-encoded query string for the recent +
// cursor path. RFC3339 timestamps contain `:` and may contain `+`, both of
// which need escaping so they survive `(*url.URL).Query()` parsing on the
// server side.
func cursorQuery(recent int, before, beforeID string) string {
	v := url.Values{}
	if recent > 0 {
		v.Set("recent", strconv.Itoa(recent))
	}
	if before != "" {
		v.Set("before", before)
	}
	if beforeID != "" {
		v.Set("before_id", beforeID)
	}
	return v.Encode()
}

// commentListFixture seeds an issue with a known comment graph for the
// thread / recent / cursor tests. The shape:
//
//	root1 (oldest)
//	├── r1a
//	└── r1b
//	    └── r1b1   (nested reply — defends Elon's point 2: recursive root walk)
//	root2 (newer, separate thread)
//	├── r2a
//	└── r2b (newest overall)
//
// Each comment is inserted with an explicit created_at so ordering and
// cursor behavior are deterministic.
type commentListFixture struct {
	IssueID string
	Root1   string
	R1a     string
	R1b     string
	R1b1    string
	Root2   string
	R2a     string
	R2b     string
	Base    time.Time
}

func newCommentListFixture(t *testing.T) commentListFixture {
	t.Helper()
	ctx := context.Background()

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, $3)
		RETURNING id
	`, testWorkspaceID, testUserID, "comment list fixture").Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	base := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)

	insert := func(parent *string, offset time.Duration, body string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, parent_id, created_at)
			VALUES ($1, $2, 'member', $3, $4, 'comment', $5, $6)
			RETURNING id
		`, issueID, testWorkspaceID, testUserID, body, parent, base.Add(offset)).Scan(&id); err != nil {
			t.Fatalf("insert comment %q: %v", body, err)
		}
		return id
	}

	root1 := insert(nil, 0, "root1")
	r1a := insert(&root1, 1*time.Minute, "r1a")
	r1b := insert(&root1, 2*time.Minute, "r1b")
	r1b1 := insert(&r1b, 3*time.Minute, "r1b1") // nested reply: parent is a reply, not a root
	root2 := insert(nil, 10*time.Minute, "root2")
	r2a := insert(&root2, 11*time.Minute, "r2a")
	r2b := insert(&root2, 12*time.Minute, "r2b")

	return commentListFixture{
		IssueID: issueID,
		Root1:   root1, R1a: r1a, R1b: r1b, R1b1: r1b1,
		Root2: root2, R2a: r2a, R2b: r2b,
		Base: base,
	}
}

func decodeComments(t *testing.T, body []byte) []CommentResponse {
	t.Helper()
	var resp []CommentResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode comments: %v", err)
	}
	return resp
}

func listComments(t *testing.T, issueID, query string) (*httptest.ResponseRecorder, []CommentResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	url := "/api/issues/" + issueID + "/comments"
	if query != "" {
		url += "?" + query
	}
	r := newRequest("GET", url, nil)
	r = withURLParam(r, "id", issueID)
	testHandler.ListComments(w, r)
	if w.Code != http.StatusOK {
		return w, nil
	}
	return w, decodeComments(t, w.Body.Bytes())
}

func ids(rows []CommentResponse) []string {
	out := make([]string, len(rows))
	for i, c := range rows {
		out[i] = c.ID
	}
	return out
}

func eqIDs(t *testing.T, got, want []string, ctx string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: ids len got=%d want=%d\ngot=%v\nwant=%v", ctx, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: ids[%d] got=%s want=%s\ngot=%v\nwant=%v", ctx, i, got[i], want[i], got, want)
		}
	}
}

// TestListComments_DefaultPreservesChronologicalOrder is a guard against
// silent regressions in the unparameterized list path — agents and the UI
// both depend on chronological order.
func TestListComments_DefaultPreservesChronologicalOrder(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newCommentListFixture(t)

	_, rows := listComments(t, fx.IssueID, "")
	want := []string{fx.Root1, fx.R1a, fx.R1b, fx.R1b1, fx.Root2, fx.R2a, fx.R2b}
	eqIDs(t, ids(rows), want, "default order")
}

// TestListComments_ThreadResolvesFromAnyAnchor proves Elon's point 2:
// regardless of whether the anchor is a root, a direct reply, or a nested
// reply (parent_id points at another reply), the server walks up to the
// thread root and returns root + every descendant.
func TestListComments_ThreadResolvesFromAnyAnchor(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newCommentListFixture(t)

	wantThread1 := []string{fx.Root1, fx.R1a, fx.R1b, fx.R1b1}

	t.Run("anchor is root", func(t *testing.T) {
		_, rows := listComments(t, fx.IssueID, "thread="+fx.Root1)
		eqIDs(t, ids(rows), wantThread1, "anchor=root1")
	})

	t.Run("anchor is direct reply", func(t *testing.T) {
		_, rows := listComments(t, fx.IssueID, "thread="+fx.R1a)
		eqIDs(t, ids(rows), wantThread1, "anchor=r1a (direct reply)")
	})

	t.Run("anchor is nested reply", func(t *testing.T) {
		// r1b1.parent_id = r1b, which itself is a reply. The recursive CTE
		// must climb root1 → r1b → r1b1 to resolve the root.
		_, rows := listComments(t, fx.IssueID, "thread="+fx.R1b1)
		eqIDs(t, ids(rows), wantThread1, "anchor=r1b1 (nested reply)")
	})

	t.Run("anchor in other thread returns only that thread", func(t *testing.T) {
		_, rows := listComments(t, fx.IssueID, "thread="+fx.R2a)
		eqIDs(t, ids(rows), []string{fx.Root2, fx.R2a, fx.R2b}, "anchor=r2a")
	})
}

// TestListComments_ThreadAnchorErrors covers the user-facing error surface
// for the thread path. The unknown-anchor case is what catches the typical
// "agent pasted a stale UUID" footgun — the server returns 404 instead of
// silently returning an empty list (which would otherwise be
// indistinguishable from a deleted thread).
func TestListComments_ThreadAnchorErrors(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newCommentListFixture(t)

	t.Run("non-uuid thread returns 400", func(t *testing.T) {
		w, _ := listComments(t, fx.IssueID, "thread=not-a-uuid")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("unknown thread anchor returns 404", func(t *testing.T) {
		w, _ := listComments(t, fx.IssueID, "thread=00000000-0000-0000-0000-000000000001")
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// TestListComments_RecentReturnsMostRecentInChronologicalOrder verifies
// the recent path returns the newest N comments but reorders them to
// chronological so the response shape matches the default list. Agents
// can feed it to a prompt verbatim.
func TestListComments_RecentReturnsMostRecentInChronologicalOrder(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newCommentListFixture(t)

	_, rows := listComments(t, fx.IssueID, "recent=3")
	// 7 comments total; newest 3 are r2a, r2b plus root2 — but ordered DESC
	// the newest 3 are r2b, r2a, root2. Reversed to chronological:
	want := []string{fx.Root2, fx.R2a, fx.R2b}
	eqIDs(t, ids(rows), want, "recent=3")
}

// TestListComments_RecentWithCompositeCursor exercises Elon's point 3:
// (created_at, id) is the stable cursor — paging with --before+--before-id
// returns the next page without skipping or duplicating rows.
func TestListComments_RecentWithCompositeCursor(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newCommentListFixture(t)

	// First page: newest 3 = [root2, r2a, r2b] (chronological).
	_, page1 := listComments(t, fx.IssueID, "recent=3")
	eqIDs(t, ids(page1), []string{fx.Root2, fx.R2a, fx.R2b}, "page1")

	// Cursor = oldest row in page1 (root2). Asking for the next 3 with that
	// cursor must skip everything ≥ root2 and return [r1a, r1b, r1b1] (the
	// next 3 newest before root2, ordered chronologically).
	cursor := page1[0]
	_, page2 := listComments(t, fx.IssueID, cursorQuery(3, cursor.CreatedAt, cursor.ID))
	eqIDs(t, ids(page2), []string{fx.R1a, fx.R1b, fx.R1b1}, "page2 after cursor")
}

// TestListComments_CompositeCursorStableUnderSameTimestamp pins the actual
// purpose of the composite cursor: when two rows share a created_at the
// (created_at, id) ordering disambiguates them. A timestamp-only cursor
// would either skip one or return the same row twice across pages.
func TestListComments_CompositeCursorStableUnderSameTimestamp(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, $3) RETURNING id
	`, testWorkspaceID, testUserID, "tie-break fixture").Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	ts := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Millisecond)
	insert := func(body string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, created_at)
			VALUES ($1, $2, 'member', $3, $4, 'comment', $5) RETURNING id
		`, issueID, testWorkspaceID, testUserID, body, ts).Scan(&id); err != nil {
			t.Fatalf("insert: %v", err)
		}
		return id
	}
	a := insert("a")
	b := insert("b")
	c := insert("c")

	// All three share `ts`; the total order is (ts, id) which lexicographic
	// ordering of the UUID strings determines. Pull all three to learn the
	// canonical order, then page through with size 1 to assert no skip / dup.
	_, all := listComments(t, issueID, "")
	if len(all) != 3 {
		t.Fatalf("seed: expected 3 comments, got %d", len(all))
	}
	want := ids(all) // canonical chronological order

	_, page1 := listComments(t, issueID, "recent=1")
	if len(page1) != 1 {
		t.Fatalf("page1: expected 1, got %d", len(page1))
	}
	got := []string{page1[0].ID}

	cursor := page1[0]
	for i := 0; i < 2; i++ {
		_, page := listComments(t, issueID, cursorQuery(1, cursor.CreatedAt, cursor.ID))
		if len(page) != 1 {
			t.Fatalf("page %d: expected 1, got %d", i+2, len(page))
		}
		got = append(got, page[0].ID)
		cursor = page[0]
	}

	// Newest-first walk gives the reverse of canonical chronological order.
	wantReverse := []string{want[2], want[1], want[0]}
	_ = a
	_ = b
	_ = c
	eqIDs(t, got, wantReverse, "paginated walk")
}

// TestListComments_FlagCombinationRules locks Elon's point 4. The matrix is
// tiny on purpose — the goal is to ensure conflicting flags are rejected
// loudly at the API surface so the CLI's local validation cannot drift.
func TestListComments_FlagCombinationRules(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newCommentListFixture(t)

	cases := []struct {
		name   string
		query  string
		status int
	}{
		{
			name:   "thread + recent rejected",
			query:  "thread=" + fx.Root1 + "&recent=5",
			status: http.StatusBadRequest,
		},
		{
			name: "thread + before rejected",
			query: (func() string {
				v := url.Values{}
				v.Set("thread", fx.Root1)
				v.Set("before", time.Now().UTC().Format(time.RFC3339))
				v.Set("before_id", uuid.NewString())
				return v.Encode()
			})(),
			status: http.StatusBadRequest,
		},
		{
			name: "before without before_id rejected",
			query: (func() string {
				v := url.Values{}
				v.Set("recent", "5")
				v.Set("before", time.Now().UTC().Format(time.RFC3339))
				return v.Encode()
			})(),
			status: http.StatusBadRequest,
		},
		{
			name: "before_id without before rejected",
			query: (func() string {
				v := url.Values{}
				v.Set("recent", "5")
				v.Set("before_id", uuid.NewString())
				return v.Encode()
			})(),
			status: http.StatusBadRequest,
		},
		{
			name:   "zero recent rejected",
			query:  "recent=0",
			status: http.StatusBadRequest,
		},
		{
			name:   "negative recent rejected",
			query:  "recent=-3",
			status: http.StatusBadRequest,
		},
		{
			name:   "non-numeric recent rejected",
			query:  "recent=lots",
			status: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := listComments(t, fx.IssueID, tc.query)
			if w.Code != tc.status {
				t.Fatalf("query=%q\n  got=%d want=%d body=%s", tc.query, w.Code, tc.status, w.Body.String())
			}
		})
	}
}

// TestListComments_ThreadWithSinceFiltersWithinThread proves the allowed
// combination from the rules: `thread + since` returns only comments in
// that thread newer than `since`. The since filter is applied in-memory
// after the thread CTE so the root membership semantics stay intact.
func TestListComments_ThreadWithSinceFiltersWithinThread(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newCommentListFixture(t)

	// since = base+1m30s → drop root1, r1a; keep r1b, r1b1.
	v := url.Values{}
	v.Set("thread", fx.Root1)
	v.Set("since", fx.Base.Add(90*time.Second).UTC().Format(time.RFC3339Nano))
	_, rows := listComments(t, fx.IssueID, v.Encode())
	eqIDs(t, ids(rows), []string{fx.R1b, fx.R1b1}, "thread+since")
}
