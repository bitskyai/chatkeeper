package search

import (
	"context"
	"github.com/bitskyai/chatkeeper/internal/core"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"time"
)

// FromPlugin obtains the content adapter and stable extraction identity behind
// the same bounded panic boundary used for the adapter's read operations.
func FromPlugin(ctx context.Context, path string, p pluginapi.Plugin) (Source, error) {
	src, e := core.Call(ctx, 10*time.Second, func(context.Context) (Source, error) {
		m := p.Manifest()
		return Source{Path: path, Content: p.Content(), Identity: m.ID + "@" + m.Version}, nil
	})
	if e != nil {
		return Source{}, ErrUnavailable
	}
	return src, nil
}
