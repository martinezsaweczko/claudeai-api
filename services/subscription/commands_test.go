package subscription

import "testing"

func TestCommandPatterns(t *testing.T) {
	svc, _ := newTestService(&fakeStore{})

	tests := []struct {
		name string
		body string
		want string // name of the first matching command, empty when none matches
	}{
		{name: "create short", body: "subs: marca", want: "subs_create"},
		{name: "create long with accent", body: "Subscripción: marca", want: "subs_create"},
		{name: "delete short", body: "del_subs", want: "subs_delete"},
		{name: "delete long verb", body: "delete_subs", want: "subs_delete"},
		{name: "delete long noun", body: "del_subscripcion", want: "subs_delete"},
		{name: "delete long noun with accent", body: "delete_subscripción", want: "subs_delete"},
		{name: "delete uppercase", body: "DEL_SUBS", want: "subs_delete"},
		{name: "list short", body: "list_subs", want: "subs_list"},
		{name: "list long noun", body: "list_subscripcion", want: "subs_list"},
		{name: "list uppercase", body: "LIST_SUBS", want: "subs_list"},
		{name: "delete missing first letter", body: "del_ubs", want: ""},
		{name: "list missing first letter", body: "list_ubs", want: ""},
		{name: "unrelated text", body: "hola", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			for _, cmd := range svc.Commands() {
				if cmd.Pattern.MatchString(tt.body) {
					got = cmd.Name
					break
				}
			}
			if got != tt.want {
				t.Errorf("first command matching %q = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}
