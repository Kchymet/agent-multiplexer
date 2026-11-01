package main

import (
	"reflect"
	"testing"

	"amux/internal/core"
)

func TestGrantsActionForms(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		fields map[string]string
		label  string
	}{
		{[]string{"--all"}, map[string]string{"repos": "", "all": "1"}, "all tracked repos"},
		{[]string{"--none"}, map[string]string{"repos": ""}, "none"},
		{nil, map[string]string{"repos": ""}, "none"},
		{[]string{"api", "web"}, map[string]string{"repos": "api,web"}, "api, web"},
	} {
		action, label, err := grantsAction("wg", tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if action.Action != core.ActionCoordinatorSetRepos || action.ID != "wg" || !reflect.DeepEqual(action.Fields, tc.fields) || label != tc.label {
			t.Fatalf("%v = %+v %q, want fields %v label %q", tc.args, action, label, tc.fields, tc.label)
		}
	}
	for _, bad := range [][]string{{"--all", "api"}, {"api", "--none"}, {"--bogus"}} {
		if _, _, err := grantsAction("wg", bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}
