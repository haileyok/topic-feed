package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Publisher writes an app.bsky.feed.generator record for every configured feed into the
// owner's repo, which is what makes the feeds show up in Bluesky.
type Publisher struct {
	Dir        identity.Directory
	OwnerDID   syntax.DID
	ServiceDID string
	Out        io.Writer
}

// Publish writes (or, with login nil, only prints) each feed's record. Fields already on
// an existing record that the config doesn't set, such as an avatar added in the app,
// are kept, as is its createdAt. Records in the repo for feeds that aren't in the config
// are listed but left alone.
func (p *Publisher) Publish(ctx context.Context, feeds []Feed, login *atclient.APIClient) error {
	ident, err := p.Dir.LookupDID(ctx, p.OwnerDID)
	if err != nil {
		return fmt.Errorf("resolve owner %s: %w", p.OwnerDID, err)
	}
	pds := ident.PDSEndpoint()
	if pds == "" {
		return fmt.Errorf("owner %s has no PDS endpoint", p.OwnerDID)
	}
	if login != nil && (login.AccountDID == nil || *login.AccountDID != p.OwnerDID) {
		return fmt.Errorf("logged in as %v, but the feeds belong to %s", login.AccountDID, p.OwnerDID)
	}
	public := atclient.NewAPIClient(pds)

	for _, f := range feeds {
		existing, err := p.getRecord(ctx, public, f.Rkey)
		if err != nil {
			return fmt.Errorf("feed %s: %w", f.Rkey, err)
		}
		rec := map[string]any{}
		for k, v := range existing {
			rec[k] = v
		}
		rec["$type"] = generatorCollection
		rec["did"] = p.ServiceDID
		rec["displayName"] = f.DisplayName
		rec["description"] = f.Description
		if f.AcceptsInteractions {
			rec["acceptsInteractions"] = true
		} else {
			delete(rec, "acceptsInteractions")
		}
		if _, ok := rec["createdAt"].(string); !ok {
			rec["createdAt"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		}
		uri := "at://" + p.OwnerDID.String() + "/" + generatorCollection + "/" + f.Rkey
		action := "create"
		if existing != nil {
			action = "update"
		}
		if login == nil {
			b, _ := json.MarshalIndent(rec, "  ", "  ")
			fmt.Fprintf(p.Out, "would %s %s\n  %s\n", action, uri, b)
			continue
		}
		var out struct {
			URI string `json:"uri"`
			CID string `json:"cid"`
		}
		err = login.Post(ctx, syntax.NSID("com.atproto.repo.putRecord"), map[string]any{
			"repo": p.OwnerDID.String(), "collection": generatorCollection, "rkey": f.Rkey, "record": rec,
		}, &out)
		if err != nil {
			return fmt.Errorf("feed %s: put record: %w", f.Rkey, err)
		}
		fmt.Fprintf(p.Out, "%sd %s (cid %s)\n", action, out.URI, out.CID)
	}

	others, err := p.otherRecords(ctx, public, feeds)
	if err != nil {
		return err
	}
	for _, uri := range others {
		fmt.Fprintf(p.Out, "not published from here (a topic feed is published from /feeds), left as is: %s\n", uri)
	}
	return nil
}

// getRecord returns a feed's current record value, or nil when there is none.
func (p *Publisher) getRecord(ctx context.Context, c *atclient.APIClient, rkey string) (map[string]any, error) {
	var out struct {
		Value map[string]any `json:"value"`
	}
	err := c.Get(ctx, syntax.NSID("com.atproto.repo.getRecord"), map[string]any{
		"repo": p.OwnerDID.String(), "collection": generatorCollection, "rkey": rkey,
	}, &out)
	var apiErr *atclient.APIError
	if errors.As(err, &apiErr) && apiErr.Name == "RecordNotFound" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get record: %w", err)
	}
	return out.Value, nil
}

// otherRecords lists generator records in the owner's repo whose rkey isn't configured.
func (p *Publisher) otherRecords(ctx context.Context, c *atclient.APIClient, feeds []Feed) ([]string, error) {
	var out struct {
		Records []struct {
			URI string `json:"uri"`
		} `json:"records"`
	}
	err := c.Get(ctx, syntax.NSID("com.atproto.repo.listRecords"), map[string]any{
		"repo": p.OwnerDID.String(), "collection": generatorCollection, "limit": 100,
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	var others []string
	for _, r := range out.Records {
		u, err := syntax.ParseATURI(r.URI)
		if err != nil {
			continue
		}
		if !slices.ContainsFunc(feeds, func(f Feed) bool { return f.Rkey == u.RecordKey().String() }) {
			others = append(others, r.URI)
		}
	}
	return others, nil
}
