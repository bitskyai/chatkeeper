package slack

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var patterns = map[string]*regexp.Regexp{"team": regexp.MustCompile(`^T[A-Z0-9]+$`), "channel": regexp.MustCompile(`^[CG][A-Z0-9]+$`), "user": regexp.MustCompile(`^[UW][A-Z0-9]+$`), "file": regexp.MustCompile(`^F[A-Z0-9]+$`), "ts": regexp.MustCompile(`^[0-9]+\.[0-9]{6}$`)}

func id(v any, kind string) bool { s, ok := v.(string); return ok && patterns[kind].MatchString(s) }
func str(v any) string           { s, _ := v.(string); return s }
func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, e := n.Int64()
		return i, e == nil
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), n == float64(int64(n))
	}
	return 0, false
}
func cleanURL(raw string) (*url.URL, error) {
	if strings.ContainsAny(raw, "\\ \t\n\r") || strings.IndexFunc(raw, func(r rune) bool { return r < 32 }) >= 0 {
		return nil, pluginapi.ErrInvalid
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.Port() != "" && u.Port() != "443" {
		return nil, pluginapi.ErrInvalid
	}
	return u, nil
}
func FileURL(raw string) error {
	u, e := cleanURL(raw)
	if e != nil || u.Hostname() != "files.slack.com" {
		return pluginapi.ErrInvalid
	}
	return nil
}
func Permalink(raw, channel string, root bool) error {
	u, e := cleanURL(raw)
	if e != nil {
		return e
	}
	if !regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+slack\.com$`).MatchString(u.Hostname()) || !regexp.MustCompile(`^/archives/`+regexp.QuoteMeta(channel)+`/p[0-9]+$`).MatchString(u.Path) || u.RawQuery == "" && strings.Contains(raw, "?") {
		return pluginapi.ErrInvalid
	}
	seen := map[string]bool{}
	if u.RawQuery != "" {
		for _, field := range strings.Split(u.RawQuery, "&") {
			k, v, ok := strings.Cut(field, "=")
			if !ok || seen[k] || v == "" {
				return pluginapi.ErrInvalid
			}
			seen[k] = true
			switch k {
			case "cid":
				if v != channel || !id(v, "channel") {
					return pluginapi.ErrInvalid
				}
			case "thread_ts":
				if !id(v, "ts") || root && pathLast(u.Path) != "p"+strings.ReplaceAll(v, ".", "") {
					return pluginapi.ErrInvalid
				}
			default:
				return pluginapi.ErrInvalid
			}
		}
	}
	return nil
}
func pathLast(p string) string { i := strings.LastIndex(p, "/"); return p[i+1:] }
func label(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)), " ")
}
func list(m map[string]any, k string) ([]any, error) {
	v, ok := m[k]
	if !ok {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok || len(a) > 100 {
		return nil, pluginapi.ErrInvalid
	}
	return a, nil
}

// Content extracts bounded text and explicit files. Preview media is retained raw,
// but never interpreted as a resource to fetch or embed.
func Content(message pluginapi.Message) ([]map[string]any, []string, error) {
	files := []map[string]any{}
	var texts []string
	budget := 1000
	spend := func(depth int) error {
		budget--
		if budget < 0 || depth > 12 {
			return pluginapi.ErrInvalid
		}
		return nil
	}
	fileList := func(c map[string]any) error {
		a, e := list(c, "files")
		if e != nil {
			return e
		}
		for _, v := range a {
			m, ok := v.(map[string]any)
			if !ok {
				return pluginapi.ErrInvalid
			}
			files = append(files, m)
		}
		if len(files) > 100 {
			return pluginapi.ErrInvalid
		}
		return nil
	}
	var cosmetic func(any, int) error
	cosmetic = func(v any, d int) error {
		if e := spend(d); e != nil {
			return e
		}
		switch n := v.(type) {
		case map[string]any:
			for _, v := range n {
				if e := cosmetic(v, d+1); e != nil {
					return e
				}
			}
		case []any:
			for _, v := range n {
				if e := cosmetic(v, d+1); e != nil {
					return e
				}
			}
		}
		return nil
	}
	known := map[string]bool{}
	for _, s := range strings.Fields("section context header divider plain_text mrkdwn rich_text rich_text_section rich_text_list rich_text_quote rich_text_preformatted text link user channel emoji broadcast image button") {
		known[s] = true
	}
	labels := []string{"text", "url", "name", "user_id", "channel_id", "alt_text", "title", "label", "description", "fallback", "accessibility_label"}
	media := map[string]bool{"image_url": true, "thumb_url": true, "video_url": true, "audio_url": true, "video_html": true, "slack_file": true, "files": true}
	var visit func(any, int) error
	visit = func(v any, depth int) error {
		if e := spend(depth); e != nil {
			return e
		}
		if a, ok := v.([]any); ok {
			for _, v := range a {
				if e := visit(v, depth+1); e != nil {
					return e
				}
			}
			return nil
		}
		n, ok := v.(map[string]any)
		if !ok {
			return pluginapi.ErrInvalid
		}
		kind := ""
		if v, has := n["type"]; has {
			var valid bool
			kind, valid = v.(string)
			if !valid {
				return pluginapi.ErrInvalid
			}
		}
		switch kind {
		case "file":
			return pluginapi.ErrInvalid
		case "canvas":
			if len(n) != 2 || !id(n["file_id"], "file") {
				return pluginapi.ErrInvalid
			}
			texts = append(texts, "canvas "+str(n["file_id"]))
			return nil
		case "usergroup":
			if !regexp.MustCompile(`^S[A-Z0-9]+$`).MatchString(str(n["usergroup_id"])) {
				return pluginapi.ErrInvalid
			}
			texts = append(texts, "@"+str(n["usergroup_id"]))
			return nil
		case "date":
			stamp, ok := integer(n["timestamp"])
			if !ok || stamp < 0 {
				return pluginapi.ErrInvalid
			}
			fallback, has := n["fallback"]
			if !has {
				fallback = "date " + strconv.FormatInt(stamp, 10)
			}
			s, ok := fallback.(string)
			if !ok {
				return pluginapi.ErrInvalid
			}
			texts = append(texts, label(s))
			return nil
		case "message_mention":
			allowed := map[string]bool{"type": true, "message_ts": true, "channel_id": true, "author_id": true, "thread_ts": true, "text": true, "url": true}
			for k := range n {
				if !allowed[k] {
					return pluginapi.ErrInvalid
				}
			}
			if !id(n["message_ts"], "ts") || !id(n["channel_id"], "channel") {
				return pluginapi.ErrInvalid
			}
			for k, kind := range map[string]string{"author_id": "user", "thread_ts": "ts"} {
				if v, ok := n[k]; ok && !id(v, kind) {
					return pluginapi.ErrInvalid
				}
			}
			text, link := "", ""
			for k, target := range map[string]*string{"text": &text, "url": &link} {
				if v, has := n[k]; has {
					s, ok := v.(string)
					if !ok {
						return pluginapi.ErrInvalid
					}
					*target = s
				}
			}
			text = label(text)
			if text == "" {
				text = "Slack message"
			}
			if Permalink(link, str(n["channel_id"]), false) == nil {
				text = "<" + link + "|" + text + ">"
			}
			texts = append(texts, text)
			return nil
		}
		if e := fileList(n); e != nil {
			return e
		}
		done := map[string]bool{}
		for _, k := range labels {
			v, has := n[k]
			if !has {
				continue
			}
			s, ok := v.(string)
			if !ok {
				switch v.(type) {
				case map[string]any:
					if e := visit(v, depth+1); e != nil {
						return e
					}
					done[k] = true
					continue
				default:
					return pluginapi.ErrInvalid
				}
			}
			done[k] = true
			if k == "user_id" && kind == "user" && id(s, "user") {
				texts = append(texts, "<@"+s+">")
			} else if s != "" {
				if !known[kind] {
					s = label(s)
				}
				texts = append(texts, s)
			}
		}
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := n[k]
			if done[k] || media[k] {
				continue
			}
			if k == "style" {
				if _, ok := v.(map[string]any); !ok {
					return pluginapi.ErrInvalid
				}
				if e := cosmetic(v, depth+1); e != nil {
					return e
				}
				continue
			}
			switch v.(type) {
			case map[string]any, []any:
				if k == "accessory" {
					m, ok := v.(map[string]any)
					if !ok || str(m["type"]) == "" {
						return pluginapi.ErrInvalid
					}
				}
				if e := visit(v, depth+1); e != nil {
					return e
				}
			}
		}
		return nil
	}
	blocks := func(c map[string]any, depth int) error {
		a, e := list(c, "blocks")
		if e != nil {
			return e
		}
		for _, v := range a {
			m, ok := v.(map[string]any)
			if !ok || str(m["type"]) == "" {
				return pluginapi.ErrInvalid
			}
			if e = visit(v, depth); e != nil {
				return e
			}
		}
		return nil
	}
	var attachments func(map[string]any, int) error
	attachments = func(c map[string]any, depth int) error {
		a, e := list(c, "attachments")
		if e != nil {
			return e
		}
		for _, v := range a {
			if e = spend(depth); e != nil {
				return e
			}
			m, ok := v.(map[string]any)
			if !ok {
				return pluginapi.ErrInvalid
			}
			for _, k := range []string{"pretext", "author_name", "title", "text", "fallback", "footer"} {
				if v, ok := m[k]; ok {
					s, valid := v.(string)
					if !valid {
						return pluginapi.ErrInvalid
					}
					if s != "" {
						texts = append(texts, s)
					}
				}
			}
			fields, e := list(m, "fields")
			if e != nil {
				return e
			}
			for _, f := range fields {
				if e = spend(depth + 1); e != nil {
					return e
				}
				fm, ok := f.(map[string]any)
				if !ok {
					return pluginapi.ErrInvalid
				}
				for _, k := range []string{"title", "value"} {
					if v, ok := fm[k]; ok {
						s, ok := v.(string)
						if !ok {
							return pluginapi.ErrInvalid
						}
						if s != "" {
							texts = append(texts, s)
						}
					}
				}
			}
			if e = fileList(m); e != nil {
				return e
			}
			if e = blocks(m, depth+1); e != nil {
				return e
			}
			if e = attachments(m, depth+1); e != nil {
				return e
			}
		}
		return nil
	}
	if e := fileList(message); e != nil {
		return nil, nil, e
	}
	if e := blocks(message, 0); e != nil {
		return nil, nil, e
	}
	if e := attachments(message, 0); e != nil {
		return nil, nil, e
	}
	return files, texts, nil
}

var mentionPattern = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^<>]*)?>`)

func Mentioned(m pluginapi.Message) []string {
	_, texts, e := Content(m)
	if e != nil {
		return nil
	}
	texts = append(texts, str(m["text"]))
	set := map[string]bool{}
	for _, s := range texts {
		for _, hit := range mentionPattern.FindAllStringSubmatch(s, -1) {
			set[hit[1]] = true
		}
	}
	out := []string{}
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func Name(user pluginapi.Message) string {
	p, _ := user["profile"].(map[string]any)
	for _, v := range []any{p["display_name"], p["real_name"], user["real_name"], user["name"], user["id"]} {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "unknown"
}

type Hooks struct{}

func (Hooks) Author(_ context.Context, user pluginapi.Message) (string, error) {
	return Name(user), nil
}
func (Hooks) Text(_ context.Context, m pluginapi.Message, users map[string]pluginapi.Message) (string, error) {
	_, texts, e := Content(m)
	if e != nil {
		return "", e
	}
	if text := str(m["text"]); text != "" {
		texts = append([]string{text}, texts...)
	}
	out := strings.Join(texts, "\n")
	out = mentionPattern.ReplaceAllStringFunc(out, func(token string) string {
		id := mentionPattern.FindStringSubmatch(token)[1]
		if user := users[id]; user != nil {
			return "@" + Name(user)
		}
		return "@" + id
	})
	return out, nil
}
func (Hooks) Validate(ctx context.Context, v pluginapi.Snapshot) error {
	m := v.Manifest
	if (m.Schema != 1 && m.Schema != 2) || m.Platform != "" && m.Platform != "slack" || !id(m.TeamID, "team") || !id(m.Channel, "channel") || !id(m.RootTS, "ts") || Permalink(m.Permalink, m.Channel, true) != nil || len(v.Messages) == 0 || len(v.Messages) != m.MessageCount {
		return pluginapi.ErrInvalid
	}
	files := map[string]bool{}
	seen := map[string]bool{}
	for i, msg := range v.Messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !id(msg["ts"], "ts") || !id(msg["user"], "user") || seen[str(msg["ts"])] || i == 0 && msg["ts"] != m.RootTS {
			return pluginapi.ErrInvalid
		}
		seen[str(msg["ts"])] = true
		thread, has := msg["thread_ts"]
		if has && thread != m.RootTS || i > 0 && !has {
			return pluginapi.ErrInvalid
		}
		if text, has := msg["text"]; has {
			if _, ok := text.(string); !ok {
				return pluginapi.ErrInvalid
			}
		}
		user := v.Users[str(msg["user"])]
		if user == nil || user["id"] != msg["user"] {
			return pluginapi.ErrInvalid
		}
		if profile, has := user["profile"]; has {
			if _, ok := profile.(map[string]any); !ok {
				return pluginapi.ErrInvalid
			}
		}
		entries, _, e := Content(msg)
		if e != nil {
			return e
		}
		for _, f := range entries {
			if !id(f["id"], "file") {
				return pluginapi.ErrInvalid
			}
			files[str(f["id"])] = true
		}
	}
	count := int64(0)
	if v, has := v.Messages[0]["reply_count"]; has {
		var ok bool
		count, ok = integer(v)
		if !ok {
			return pluginapi.ErrInvalid
		}
	}
	if count < 0 || m.Schema == 1 && (count != int64(len(v.Messages)-1) || len(files) != len(m.Files)) || m.Schema == 2 && (count > int64(len(v.Messages)-1) || len(files) > len(m.Files)) || len(v.Blobs) != len(m.Files) {
		return pluginapi.ErrInvalid
	}
	seen = map[string]bool{}
	for _, f := range m.Files {
		b, ok := v.Blobs[f.Path]
		if m.Schema == 1 && !files[f.ID] || seen[f.ID] || !ok || f.Path != "files/"+f.ID+".bin" || FileURL(f.URL) != nil || b.Size != f.Size || b.SHA256 != f.SHA256 {
			return pluginapi.ErrInvalid
		}
		seen[f.ID] = true
	}
	return nil
}
func Event(raw pluginapi.Message) pluginapi.Event {
	kind := str(raw["type"])
	var channel, ts, typ string
	switch kind {
	case "reaction_added", "reaction_removed":
		item, ok := raw["item"].(map[string]any)
		if !ok || item["type"] != "message" {
			return pluginapi.Event{}
		}
		channel, ts, typ = str(item["channel"]), str(item["ts"]), "reaction"
	case "message":
		message := map[string]any(raw)
		if raw["subtype"] == "message_changed" {
			message, _ = raw["message"].(map[string]any)
		} else if raw["subtype"] == "message_deleted" {
			message, _ = raw["previous_message"].(map[string]any)
		}
		channel, ts, typ = str(raw["channel"]), str(message["thread_ts"]), "message"
		if ts == "" {
			ts = str(message["ts"])
		}
	default:
		return pluginapi.Event{}
	}
	if !id(channel, "channel") || !id(ts, "ts") {
		return pluginapi.Event{}
	}
	return pluginapi.Event{Channel: channel, RootTS: ts, Kind: typ}
}
