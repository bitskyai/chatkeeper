package slack

import (
	"context"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"reflect"
	"strings"
	"testing"
)

func msg(raw string) pluginapi.Message {
	var m pluginapi.Message
	if e := json.Unmarshal([]byte(raw), &m); e != nil {
		panic(e)
	}
	return m
}
func TestContent(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
		bad       bool
	}{
		{"canvas", `{"blocks":[{"type":"canvas","file_id":"FCANVAS"}]}`, []string{"canvas FCANVAS"}, false},
		{"extra canvas fields", `{"blocks":[{"type":"canvas","file_id":"F1","url":"https://example.invalid"}]}`, []string{"canvas F1"}, false},
		{"file block", `{"blocks":[{"type":"file","external_id":"x"}]}`, []string{"file x"}, false},
		{"preview", `{"attachments":[{"title":"Title","text":"Preview","image_url":"https://example.invalid","video_html":"<iframe>"}]}`, []string{"Title", "Preview"}, false},
		{"future", `{"blocks":[{"type":"future_widget","text":"<script>widget</script>","metadata":{"label":"A label"}}]}`, []string{"&lt;script&gt;widget&lt;/script&gt;", "A label"}, false},
		{"style", `{"blocks":[{"type":"text","text":"Keep","style":{"image_url":"https://example.invalid","label":"ignored","nested":{"flag":true}}}]}`, []string{"Keep"}, false},
		{"odd style", `{"blocks":[{"type":"text","text":"x","style":[]}]}`, []string{"x"}, false},
		{"list style string", `{"blocks":[{"type":"rich_text","elements":[{"type":"rich_text_list","style":"bullet","indent":0,"border":0,"elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"first"}]},{"type":"rich_text_section","elements":[{"type":"text","text":"second","style":{"bold":true}}]}]}]}]}`, []string{"first", "second"}, false},
		{"button style string", `{"blocks":[{"type":"actions","elements":[{"type":"button","style":"primary","text":{"type":"plain_text","text":"Review"},"url":"https://example.invalid/pr/1"}]}]}`, []string{"Review", "https://example.invalid/pr/1"}, false},
		{"bad type", `{"blocks":[{"type":[]}]}`, nil, false},
		{"bad text", `{"blocks":[{"type":"text","text":[]}]}`, nil, false},
		{"accessory", `{"blocks":[{"type":"section","accessory":{}}]}`, nil, false},
		{"bad files", `{"files":[null]}`, nil, false},
		{"bad attachments", `{"attachments":[null]}`, nil, false},
		{"null attachment fields", `{"attachments":[{"title":null,"text":"PR opened","fields":[{"title":"Repo","value":null},null]}]}`, []string{"PR opened", "Repo"}, false},
		{"too deep", `{"blocks":[{"type":"x","a":` + strings.Repeat(`{"a":`, 40) + `{}` + strings.Repeat(`}`, 40) + `}]}`, nil, true},
		{"too many", `{"blocks":[{"type":"x","a":[` + strings.TrimSuffix(strings.Repeat(`[],`, 60000), ",") + `]}]}`, nil, true},
		{"mention", `{"blocks":[{"type":"message_mention","message_ts":"100.000001","channel_id":"COTHER","url":"https://fixture.slack.com/archives/COTHER/p100000001","text":"<script> & label"}]}`, []string{"<https://fixture.slack.com/archives/COTHER/p100000001|&lt;script&gt; &amp; label>"}, false},
		{"unsafe mention", `{"blocks":[{"type":"message_mention","message_ts":"100.000001","channel_id":"COTHER","url":"javascript:alert(1)","text":"label <bait>"}]}`, []string{"label &lt;bait&gt;"}, false},
		{"bad mention", `{"blocks":[{"type":"message_mention","message_ts":"bad","channel_id":"COTHER"}]}`, []string{"Slack message"}, false},
		{"usergroup", `{"blocks":[{"type":"usergroup","usergroup_id":"SFIXTURE"}]}`, []string{"@SFIXTURE"}, false},
		{"date", `{"blocks":[{"type":"date","timestamp":1700000000,"fallback":"Fixture date"}]}`, []string{"Fixture date"}, false},
		{"user", `{"blocks":[{"type":"user","user_id":"URICH"}]}`, []string{"<@URICH>"}, false},
		{"image", `{"blocks":[{"type":"image","image_url":"https://example.invalid","alt_text":"An image"}]}`, []string{"An image"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, e := Content(msg(tc.raw))
			if (e != nil) != tc.bad {
				t.Fatal(e)
			}
			if !tc.bad && len(got)+len(tc.want) > 0 && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%#v want %#v", got, tc.want)
			}
		})
	}
}
func TestURLSafety(t *testing.T) {
	for _, u := range []string{"http://files.slack.com/a", "https://files.slack.com.evil.invalid/a", "https://user@files.slack.com/a", "https://files.slack.com:444/a", "https://files.slack.com/a#x", "https://files.slack.com/a\\b", "https://example.invalid/a"} {
		if FileURL(u) == nil {
			t.Fatal(u)
		}
	}
	if e := FileURL("https://files.slack.com/files-pri/T/FFILE"); e != nil {
		t.Fatal(e)
	}
	for _, suffix := range []string{"?", "?cid=CWRONG", "?cid=CFIXTURE&cid=CFIXTURE", "?bad=1", "?thread_ts=2.000001", "#x"} {
		if Permalink("https://fixture.slack.com/archives/CFIXTURE/p100000001"+suffix, "CFIXTURE", true) == nil {
			t.Fatal(suffix)
		}
	}
}
func TestEventNormalization(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want pluginapi.Event
	}{
		{`{"type":"reaction_added","item":{"type":"message","channel":"CFIXTURE","ts":"100.000001"}}`, pluginapi.Event{Channel: "CFIXTURE", RootTS: "100.000001", Kind: "reaction"}},
		{`{"type":"message","subtype":"message_changed","channel":"CFIXTURE","message":{"ts":"101.000001","thread_ts":"100.000001"}}`, pluginapi.Event{Channel: "CFIXTURE", RootTS: "100.000001", Kind: "message"}},
		{`{"type":"message","subtype":"message_deleted","channel":"CFIXTURE","previous_message":{"ts":"101.000001","thread_ts":"100.000001"}}`, pluginapi.Event{Channel: "CFIXTURE", RootTS: "100.000001", Kind: "message"}},
		{`{"type":"reaction_added","item":{"type":"file","channel":"CFIXTURE","ts":"100.000001"}}`, pluginapi.Event{}},
		{`{"type":"message","channel":"../escape","ts":"bad"}`, pluginapi.Event{}},
	} {
		got := Event(msg(tc.raw))
		if got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}
func TestNamesAndMentions(t *testing.T) {
	users := map[string]pluginapi.Message{"U1": msg(`{"id":"U1","name":"handle","real_name":"Top","profile":{"display_name":"Display","real_name":"Profile"}}`), "U2": msg(`{"id":"U2","name":"handle","real_name":"Top","profile":{"real_name":"Profile"}}`), "U3": msg(`{"id":"U3","name":"handle","real_name":"Top"}`), "U4": msg(`{"id":"U4","name":"handle"}`), "U5": msg(`{"id":"U5"}`), "UBAD": msg(`{"id":"UBAD","profile":{"display_name":7},"real_name":"Fallback"}`)}
	for id, want := range map[string]string{"U1": "Display", "U2": "Profile", "U3": "Top", "U4": "handle", "U5": "U5", "UBAD": "Fallback"} {
		if Name(users[id]) != want {
			t.Fatal(id, Name(users[id]))
		}
	}
	s, e := (Hooks{}).Text(context.Background(), msg(`{"text":"ask <@U1|stale> <@UNKNOWN>","blocks":[{"type":"user","user_id":"U2"}]}`), users)
	if e != nil || !strings.Contains(s, "@Display") || strings.Contains(s, "stale") || !strings.Contains(s, "@Profile") {
		t.Fatal(s, e)
	}
}
