package hostbackend

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"context"

	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/queue"
)

// ErrQueueUnsupported means the daemon predates the user question queue (it
// answers QueryQueue or the item queries as unknown). Nothing was changed.
var ErrQueueUnsupported = errors.New("the amux daemon predates the question queue; upgrade and restart amux")

// fieldQuerier is the daemon client's query with request fields
// (daemon.Client.QueryFields). A test connection without it cannot serve the
// queue.
type fieldQuerier interface {
	QueryFields(name string, fields map[string]string) (json.RawMessage, error)
}

func queryFields(c Conn, name string, fields map[string]string, dst any) error {
	q, ok := c.(fieldQuerier)
	if !ok {
		return ErrQueueUnsupported
	}
	raw, err := q.QueryFields(name, fields)
	if err != nil {
		if strings.Contains(err.Error(), "unknown query") {
			return ErrQueueUnsupported
		}
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("query %s returned no result", name)
	}
	return json.Unmarshal(raw, dst)
}

// Queue reads the user question queue as the host (the user): every open
// entry, the global inbox counts and the machine-wide summary.
func (h HostDaemon) Queue(ctx context.Context) (queue.Overview, error) {
	return call(ctx, h, func(c Conn) (queue.Overview, error) {
		var o queue.Overview
		err := queryFields(c, core.QueryQueue, nil, &o)
		return o, err
	})
}

// InboxItems lists the global inbox's active (open or acknowledged) items,
// read as the global inbox owner.
func (h HostDaemon) InboxItems(ctx context.Context) ([]items.Item, error) {
	return call(ctx, h, func(c Conn) ([]items.Item, error) {
		var l items.List
		err := queryFields(c, core.QueryItems, map[string]string{
			core.ItemBoxField: core.ItemBoxInbox, core.ItemLimitField: "200",
		}, &l)
		return l.Items, err
	})
}

// Item reads one item in any state, as the global inbox owner. ok is false
// when it does not exist.
func (h HostDaemon) Item(ctx context.Context, id string) (items.Item, bool, error) {
	type out struct {
		it items.Item
		ok bool
	}
	r, err := call(ctx, h, func(c Conn) (out, error) {
		var l items.List
		err := queryFields(c, core.QueryItems, map[string]string{core.ItemIDField: id}, &l)
		if err != nil {
			if strings.Contains(err.Error(), "no such item") {
				return out{}, nil
			}
			return out{}, err
		}
		if len(l.Items) != 1 || l.Items[0].ID != id {
			return out{}, nil
		}
		return out{it: l.Items[0], ok: true}, nil
	})
	return r.it, r.ok, err
}
