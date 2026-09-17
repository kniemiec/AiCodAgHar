package config_test

import (
	"reflect"
	"testing"

	"github.com/kniemiec/AiCodAgHar/internal/config"
)

func TestParseApprovers(t *testing.T) {
	data := []byte("# approvers for this repo\nalice\n\nbob\n  # trailing comment\ncarol  \n")
	got := config.ParseApprovers(data)
	want := []string{"alice", "bob", "carol"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseApprovers: got %v, want %v", got, want)
	}
}

func TestParseApprovers_Empty(t *testing.T) {
	if got := config.ParseApprovers([]byte("")); got != nil {
		t.Fatalf("ParseApprovers(empty): got %v, want nil", got)
	}
}
