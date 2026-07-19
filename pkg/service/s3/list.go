// list.go implements the ListObjectsV2 key-grouping semantics
// (§14): with a delimiter, keys sharing the same substring
// between the prefix and the first delimiter occurrence collapse into one
// CommonPrefixes entry — the "directory listing" SDKs and consoles rely on.
// The grouping is separated from the HTTP handler and typed against a
// minimal lister interface so it is unit-testable without a cluster.
package s3

import (
	"context"
	"strings"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// lister is the one slice of the cluster client the listing code needs;
// *client.Client satisfies it, tests substitute a fake key store.
type lister interface {
	List(ctx context.Context, prefix, cursor string, limit int) ([]client.KVEntry, string, error)
}

// listing is one grouped ListObjectsV2 page.
type listing struct {
	objects        []client.KVEntry // Contents; keys carry the full stored path
	commonPrefixes []string         // deduped, in first-seen (= lexicographic) order, relative to the bucket
	truncated      bool             // more elements exist past this page
	next           string           // continuation token: the raw stored key to resume after
}

// listPageSize is how many raw keys each backend List call fetches while
// assembling one grouped page.
const listPageSize = 1000

// collectListing walks stored keys under root+prefix (root is the bucket's
// key prefix, e.g. "/s3/mybucket/") and applies S3 ListObjectsV2 grouping:
//
//   - a key whose remainder after prefix contains the delimiter rolls up
//     into a CommonPrefixes entry ending at (and including) the first
//     delimiter occurrence; all keys sharing it collapse into one entry;
//   - every other key is returned as an object (Contents);
//   - objects and common prefixes each count as one element toward
//     maxKeys, exactly AWS's accounting;
//   - the continuation token is the last raw key consumed into the page,
//     so resuming never re-emits a common prefix (keys that roll into an
//     already-emitted group are consumed silently past the maxKeys mark).
//
// token is the client's continuation-token ("" for the first page).
func collectListing(ctx context.Context, l lister, root, prefix, delimiter, token string, maxKeys int) (listing, error) {
	var out listing
	seen := map[string]bool{} // common prefixes already emitted this page
	count := 0                // elements (objects + common prefixes) so far
	lastConsumed := ""        // raw key of the last entry folded into the page
	cursor := token
	for {
		entries, next, err := l.List(ctx, root+prefix, cursor, listPageSize)
		if err != nil {
			return listing{}, err
		}
		for _, e := range entries {
			rel := strings.TrimPrefix(e.Key, root)
			// Find the first delimiter AFTER the prefix; a hit means this
			// key is "inside a directory" and rolls up.
			group := ""
			if delimiter != "" {
				if i := strings.Index(rel[len(prefix):], delimiter); i >= 0 {
					group = rel[:len(prefix)+i+len(delimiter)]
				}
			}
			if group != "" && seen[group] {
				// Another key under a group already in this page: no new
				// element, but the resume point advances past it so the
				// group is never repeated on the next page.
				lastConsumed = e.Key
				continue
			}
			if count >= maxKeys {
				// The next distinct element would overflow the page.
				out.truncated = true
				out.next = lastConsumed
				return out, nil
			}
			if group != "" {
				seen[group] = true
				out.commonPrefixes = append(out.commonPrefixes, group)
			} else {
				out.objects = append(out.objects, e)
			}
			count++
			lastConsumed = e.Key
		}
		if next == "" {
			return out, nil // scanned everything: page complete, not truncated
		}
		cursor = next
	}
}
