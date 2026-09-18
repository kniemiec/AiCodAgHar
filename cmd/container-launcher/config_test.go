package main

import "testing"

func TestEnvInt_Default(t *testing.T) {
	if got := envInt("LAUNCHER_TEST_ENV_INT_UNSET", 42); got != 42 {
		t.Errorf("envInt() = %d, want 42", got)
	}
}

func TestEnvInt_Set(t *testing.T) {
	t.Setenv("LAUNCHER_TEST_ENV_INT_SET", "7")
	if got := envInt("LAUNCHER_TEST_ENV_INT_SET", 42); got != 7 {
		t.Errorf("envInt() = %d, want 7", got)
	}
}
