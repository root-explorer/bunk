package daemon

import (
	"reflect"
	"testing"
)

func TestParsePublicPorts(t *testing.T) {
	cases := []struct {
		in   string
		want map[int]bool
	}{
		{`[]`, map[int]bool{}},
		{`[{"Id":"a","Ports":[{"PrivatePort":80,"Type":"tcp","PublicPort":8080}]}]`,
			map[int]bool{8080: true}},
		{`[{"Id":"a","Ports":[]},{"Id":"b","Ports":[{"PublicPort":5432},{"PublicPort":0}]}]`,
			map[int]bool{5432: true}},
		{`[{"Id":"a","Ports":[{"IP":"::","PrivatePort":80,"Type":"tcp","PublicPort":9090}]}]`,
			map[int]bool{9090: true}},
	}
	for _, c := range cases {
		got, err := parsePublicPorts([]byte(c.in))
		if err != nil {
			t.Fatalf("parsePublicPorts(%s): %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parsePublicPorts(%s) = %v, want %v", c.in, got, c.want)
		}
	}
}
