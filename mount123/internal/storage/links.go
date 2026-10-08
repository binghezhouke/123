package storage

import (
	"context"
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

// rejected invalidates only the URL that actually failed. Concurrent refreshes
// share one resolver call; a recreated Remote inherits the original deadline.
func (c *Cache) downloadLink(ctx context.Context, key, rejected string, resolve ResolveURL) (cachedLink, error) {
	for {
		if err := ctx.Err(); err != nil {
			return cachedLink{}, err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return cachedLink{}, ErrClosed
		}
		if c.links == nil {
			c.links = map[string]cachedLink{}
			c.linkFlights = map[string]chan struct{}{}
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
		}
		delete(c.linkFlights, key)
		close(pending)
		c.mu.Unlock()
		return link, err
	}
}
