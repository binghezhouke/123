package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// API download links observed with seven-day signatures are reused for six
// days, leaving a one-day buffer. This cache is independent of source/index TTLs.
const downloadLinkTTL = 6 * 24 * time.Hour
const maxCachedLinks = 4096

type cachedLink struct {
	url   string
	until time.Time
}

type persistedLink struct {
	URL   string    `json:"url"`
	Until time.Time `json:"until"`
}

func linkCacheKey(key string) string { return "download-link-v1:" + key }

func (c *Cache) loadLink(key string) (cachedLink, bool) {
	h, err := c.OpenArchiveIndex(linkCacheKey(key))
	if err != nil {
		return cachedLink{}, false
	}
	defer h.Close()
	b := make([]byte, h.Size())
	_, err = h.ReadAt(b, 0)
	if err != nil {
		return cachedLink{}, false
	}
	var p persistedLink
	if json.Unmarshal(b, &p) != nil || p.URL == "" || time.Now().After(p.Until) {
		return cachedLink{}, false
	}
	return cachedLink{url: p.URL, until: p.Until}, true
}

// rejected invalidates only the URL that actually failed. Concurrent refreshes
// share one resolver call; a recreated Remote inherits the original deadline.
func (c *Cache) downloadLink(ctx context.Context, key, rejected string, resolve ResolveURL) (cachedLink, error) {
	for {
		if err := ctx.Err(); err != nil {
			return cachedLink{}, err
		}
		var disk cachedLink
		var diskOK bool
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return cachedLink{}, ErrClosed
		}
		if c.links == nil {
			c.links = map[string]cachedLink{}
			c.linkFlights = map[string]chan struct{}{}
		}
		if _, ok := c.links[key]; !ok {
			c.mu.Unlock()
			disk, diskOK = c.loadLink(key)
			c.mu.Lock()
			if diskOK {
				c.links[key] = disk
			}
		}
		if link, ok := c.links[key]; ok && link.url != rejected && time.Now().Before(link.until) {
			c.mu.Unlock()
			return link, nil
		}
		if pending := c.linkFlights[key]; pending != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return cachedLink{}, ctx.Err()
			case <-pending:
			}
			// The completed refresh may legitimately return the same URL. Reuse its
			// result instead of having every waiter invalidate it again.
			rejected = ""
			continue
		}
		delete(c.links, key)
		pending := make(chan struct{})
		c.linkFlights[key] = pending
		c.mu.Unlock()
		started := time.Now()
		url, err := resolve(ctx)
		if err == nil && url == "" {
			err = errors.New("empty download URL")
		}
		if err == nil {
			err = ctx.Err()
		}
		link := cachedLink{url: url, until: started.Add(downloadLinkTTL)}
		c.mu.Lock()
		persist := false
		if err == nil && !c.closed {
			if len(c.links) >= maxCachedLinks {
				var oldest string
				var deadline time.Time
				for k, v := range c.links {
					if oldest == "" || v.until.Before(deadline) {
						oldest, deadline = k, v.until
					}
				}
				delete(c.links, oldest)
			}
			c.links[key] = link
			persist = true
		}
		delete(c.linkFlights, key)
		close(pending)
		c.mu.Unlock()
		if persist {
			data, _ := json.Marshal(persistedLink{URL: link.url, Until: link.until})
			_ = c.ReplaceArchiveIndex(context.Background(), linkCacheKey(key), data)
		}
		return link, err
	}
}
