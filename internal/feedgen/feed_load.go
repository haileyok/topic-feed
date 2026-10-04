package feedgen

import (
	"context"
	"log/slog"
)

// LoadServedFeeds is every feed to serve: the feeds in the database, then the personal feeds of the
// config file (which are made from each viewer's likes, not from a spec, so the database does not
// hold them). ownerDID is the service owner, whose feeds have no Owner.
//
// The database, not the config file, says what topic feeds there are. A row that can't be served (it
// names a topic the taxonomy no longer has, or asks for what only the owner may) is left out and
// logged rather than failing everything.
func LoadServedFeeds(ctx context.Context, store FeedStore, cfg *Config, ownerDID string, paths map[string]bool, log *slog.Logger) ([]Feed, error) {
	stored, err := store.ListFeeds(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Feed, 0, len(stored)+len(cfg.Feeds))
	for _, s := range stored {
		f := s.Feed(ownerDID)
		if err := f.validate(paths); err != nil {
			log.Error("not serving a feed that is not valid", "owner", s.Owner, "feed", s.Rkey, "err", err)
			continue
		}
		if f.lazy() && (f.AllowAdult || f.Personal != nil) { // what the web API refuses, whoever stored it
			log.Error("not serving a feed that asks for more than its owner may", "owner", s.Owner, "feed", s.Rkey)
			continue
		}
		out = append(out, f)
	}
	for _, f := range cfg.Feeds {
		if f.Personal != nil {
			out = append(out, f)
		}
	}
	return out, nil
}
