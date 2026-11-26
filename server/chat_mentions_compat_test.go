package server

import "testing"

func TestStableAndLegacyMentionKinds(t *testing.T) {
	for _, text := range []string{"@[finding#12 example]", "@[ファインディング#12 example]", "@[漏洞#12 example]"} {
		refs, err := parseChatMentions(text)
		if err != nil || len(refs) != 1 || refs[0].Kind != "finding" || refs[0].ID != 12 {
			t.Fatalf("%q: %#v %v", text, refs, err)
		}
	}
}
