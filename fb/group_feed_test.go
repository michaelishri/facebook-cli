package fb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type groupFeedTransport func(*http.Request) (*http.Response, error)

func (f groupFeedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The initial window contains a pinned story and a section header rather than
// regular stories. The root query can expand that window without a cursor.
func TestGroupFeedExpandsRootDiscussionWindow(t *testing.T) {
	const op = "CometGroupDiscussionRootSuccessQuery"
	const initial = `{"group":{"id":"123","name":"Example","if_viewer_can_see_highlight_units":{"highlight_units":{"edges":[{"node":{"is_unit_pinned":true,"story":{"post_id":"pinned"}}}]}},"group_feed":{"edges":[{"node":{"__typename":"GroupsSectionHeaderUnit"}}],"page_info":{"end_cursor":"initial","has_next_page":true}}}}`
	const replayed = `{"data":{"group":{"id":"123","if_viewer_can_see_highlight_units":{"highlight_units":{"edges":[{"node":{"is_unit_pinned":true,"story":{"post_id":"pinned"}}}]}},"group_feed":{"edges":[{"node":{"__typename":"GroupsSectionHeaderUnit"}},{"node":{"story":{"post_id":"regular-1","creation_time":100}}},{"node":{"post_id":"regular-2","creation_time":300}},{"node":{"post_id":"regular-3","creation_time":200}}],"page_info":{"end_cursor":"next","has_next_page":true}}}}}`
	for _, mode := range []string{"success", "newest", "newest-one", "refused", "short", "exhausted", "pinned-surface"} {
		t.Run(mode, func(t *testing.T) {
			opt := GroupFeedOptions{Limit: 3}
			if strings.HasPrefix(mode, "newest") {
				opt.Sort = "newest"
			}
			if mode == "newest-one" {
				opt.Limit = 1
			}
			vars := map[string]any{"groupID": "123", "regular_stories_count": 1, "regular_stories_stream_initial_count": 1, "sortingSetting": "CHRONOLOGICAL", "__relay_internal__pv__Example": true}
			descriptor := map[string]any{"queryName": op, "queryID": "fresh-query-id", "variables": vars}
			var data any
			if err := json.Unmarshal([]byte(initial), &data); err != nil {
				t.Fatal(err)
			}
			block := map[string]any{"preloads": []any{descriptor}, "require": []any{[]any{"RelayPrefetchedStreamCache", "next", []any{}, []any{"adp_" + op + "RelayPreloader_test", map[string]any{"__bbox": map[string]any{"result": map[string]any{"data": data}}}}}}}
			raw, err := json.Marshal(block)
			if err != nil {
				t.Fatal(err)
			}
			html := `<script type="application/json" data-sjs>{"tokens":["LSD",[],{"token":"test-token"}],"__spin_r":123}</script><script type="application/json" data-sjs>` + string(raw) + `</script>`
			// Tokens are harvested from raw HTML independently of data-sjs decoding.
			c := NewClient()
			c.Cookies = "c_user=test; xs=test"
			c.Rate = 0
			c.Retries = 0
			replays := 0
			c.HTTP.Transport = groupFeedTransport(func(r *http.Request) (*http.Response, error) {
				body := html
				if r.URL.Path != "/api/graphql/" && opt.Sort == "newest" && r.URL.Query().Get("sorting_setting") != "CHRONOLOGICAL" {
					t.Error("newest did not request chronological posts from Facebook")
				}
				if r.URL.Path == "/api/graphql/" {
					replays++
					if r.Header.Get("Cookie") != c.Cookies {
						t.Error("replay lost the session")
					}
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if r.Form.Get("doc_id") != "fresh-query-id" || r.Form.Get("fb_api_req_friendly_name") != op {
						t.Error("replay did not use harvested descriptor")
					}
					var got map[string]any
					if err := json.Unmarshal([]byte(r.Form.Get("variables")), &got); err != nil {
						t.Fatal(err)
					}
					want := map[string]any{"groupID": "123", "regular_stories_count": float64(opt.Limit + 1), "regular_stories_stream_initial_count": float64(opt.Limit + 1), "sortingSetting": "CHRONOLOGICAL", "__relay_internal__pv__Example": true}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("replay variables = %#v, want %#v", got, want)
					}
					body = replayed
					if mode == "refused" {
						body = `{"errors":[{"message":"Refused","code":1675004}]}`
					}
					if mode == "short" {
						body = `{"data":` + initial + `}`
					}
					if mode == "exhausted" {
						body = `{"data":` + strings.ReplaceAll(initial, `"has_next_page":true`, `"has_next_page":false`) + `}`
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			})
			e := &Engine{c: c, tierCap: 1}
			if mode == "pinned-surface" {
				e.pin = surfaceComet
			}
			g, err := e.GroupFeedWithOptions(context.Background(), "123", opt)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "newest") {
				want := []string{"regular-2", "regular-3", "regular-1"}[:opt.Limit]
				got := make([]string, len(g.Posts))
				for i, post := range g.Posts {
					got[i] = post.ID
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("newest posts = %v, want %v", got, want)
				}
				if len(g.Missed) != 0 {
					t.Errorf("unexpected gaps: %v", g.Missed)
				}
			} else if mode == "success" {
				if len(g.Posts) != 3 || g.Posts[0].ID != "pinned" || g.Posts[1].ID != "regular-1" || g.Posts[2].ID != "regular-2" {
					t.Fatalf("wrong discussion posts: %#v", g.Posts)
				}
				if len(g.Missed) != 0 {
					t.Errorf("unexpected gaps: %v", g.Missed)
				}
				if g.Posts[1].Tier != 1 || len(g.Posts[1].Sources) == 0 {
					t.Error("discussion post lost session provenance")
				}
			} else {
				if len(g.Posts) != 1 || !g.Posts[0].Pinned {
					t.Error("fallback lost pinned story")
				}
				if mode == "exhausted" && len(g.Missed) != 0 {
					t.Error("exhausted feed incorrectly reported as incomplete")
				} else if mode != "exhausted" && len(g.Missed) == 0 {
					t.Error("incomplete feed was not reported")
				}
			}
			wantReplays := 1
			if mode == "pinned-surface" {
				wantReplays = 0
			}
			if replays != wantReplays {
				t.Errorf("replays=%d, want %d", replays, wantReplays)
			}
			if vars["regular_stories_count"] != 1 {
				t.Error("replay mutated original descriptor")
			}
		})
	}
}

func TestGroupFeedRejectsInvalidSort(t *testing.T) {
	e := &Engine{}
	_, err := e.GroupFeedWithOptions(context.Background(), "123", GroupFeedOptions{Sort: "invalid"})
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("invalid sort must be a usage error before any network request: %v", err)
	}
}
