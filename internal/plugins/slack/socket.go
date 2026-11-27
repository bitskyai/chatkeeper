package slack

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"github.com/coder/websocket"
	"net/url"
	"strings"
	"time"
)

func SocketURL(raw string) error {
	if strings.ContainsAny(raw, "\\ \t\r\n#") {
		return pluginapi.ErrInvalid
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "wss" || !strings.HasSuffix(u.Hostname(), ".slack.com") || u.User != nil || u.Port() != "" && u.Port() != "443" {
		return pluginapi.ErrInvalid
	}
	return nil
}
func (s *Session) listen(ctx context.Context, sink pluginapi.EventSink) error {
	b, e := s.client.get(ctx, "POST", "https://slack.com/api/apps.connections.open", s.appToken, maxJSON, false)
	if e != nil {
		return e
	}
	doc, e := parse(b)
	if e != nil {
		return e
	}
	rawURL := str(doc["url"])
	if e = SocketURL(rawURL); e != nil {
		return e
	}
	conn, _, e := websocket.Dial(ctx, rawURL, &websocket.DialOptions{HTTPClient: s.client.http})
	if e != nil {
		return errAPI
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxJSON)
	sink(pluginapi.Event{Kind: "backfill"})
	for {
		_, raw, e := conn.Read(ctx)
		if e != nil {
			return errAPI
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(raw, &envelope) != nil {
			continue
		}
		var ident string
		if json.Unmarshal(envelope["envelope_id"], &ident) == nil && ident != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": ident})
			ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			e = conn.Write(ackCtx, websocket.MessageText, ack)
			cancel()
			if e != nil {
				return errAPI
			}
		}
		var kind string
		_ = json.Unmarshal(envelope["type"], &kind)
		if kind == "disconnect" {
			return nil
		}
		if kind != "events_api" {
			continue
		}
		var payload struct {
			TeamID string            `json:"team_id"`
			Event  pluginapi.Message `json:"event"`
		}
		if json.Unmarshal(envelope["payload"], &payload) != nil || payload.TeamID != s.team {
			continue
		}
		event := Event(payload.Event)
		if event.Kind != "" {
			sink(event)
		}
	}
}
func (s *Session) Start(ctx context.Context, sink pluginapi.EventSink) error {
	if s.appToken == "" {
		return pluginapi.ErrUnsupported
	}
	delay := time.Second
	for ctx.Err() == nil {
		e := s.listen(ctx, sink)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e == nil {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
	return ctx.Err()
}
