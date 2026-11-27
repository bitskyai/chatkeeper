package slack

import (
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"strings"
)

// renderingNote identifies structures that only receive generic text extraction.
// Unknown source data stays in the transport document, never interpolated as HTML.
func renderingNote(m pluginapi.Message) string {
	known := map[string]bool{}
	for _, kind := range strings.Fields("section context header divider plain_text mrkdwn rich_text rich_text_section rich_text_list rich_text_quote rich_text_preformatted text link user channel emoji broadcast image button file canvas usergroup date message_mention actions input video call markdown table raw_text color team static_select multi_static_select external_select multi_external_select users_select multi_users_select conversations_select multi_conversations_select channels_select multi_channels_select overflow datepicker timepicker datetimepicker checkboxes radio_buttons rich_text_input plain_text_input email_text_input url_text_input number_input file_input workflow_button") {
		known[kind] = true
	}
	budget := maxContentNodes
	var walk func(any, int) bool
	walk = func(v any, depth int) bool {
		budget--
		if budget < 0 || depth > maxContentDepth {
			return true
		}
		switch n := v.(type) {
		case []any:
			if len(n) > maxList {
				return true
			}
			for _, v := range n {
				if walk(v, depth+1) {
					return true
				}
			}
		case map[string]any:
			if kind, has := n["type"]; has {
				s, ok := kind.(string)
				if !ok || !known[s] {
					return true
				}
			}
			for k, v := range n {
				if k == "style" {
					continue
				}
				switch v.(type) {
				case map[string]any, []any:
					if walk(v, depth+1) {
						return true
					}
				}
			}
		}
		return false
	}
	for _, k := range []string{"blocks", "attachments", "files", "reactions"} {
		if v, has := m[k]; has {
			if _, ok := v.([]any); !ok {
				return "unexpected " + k + " shape retained in raw response"
			}
		}
	}
	if walk(m["blocks"], 0) || walk(m["attachments"], 0) {
		return "unknown or bounded content rendered as best-effort text; original retained in raw response"
	}
	return ""
}
