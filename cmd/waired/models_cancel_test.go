package main

import "testing"

// PRODUCT CONTRACT (waired-agent#633): the two answers read differently.
// "I stopped a 6 GB download" and "there was nothing to stop" are
// different facts, and an operator who typed cancel by mistake has to be
// able to tell which one happened.
func TestFormatModelsCancel(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "cancelled names the job",
			body: `{"model_id":"qwen3.5-9b","job_id":"job_a761d6a4ca1a","status":"cancelled"}`,
			want: "cancelled download: model=qwen3.5-9b job=job_a761d6a4ca1a",
		},
		{
			name: "nothing in flight says so plainly",
			body: `{"model_id":"qwen3.5-9b","status":"not_downloading"}`,
			want: "no download in progress for qwen3.5-9b",
		},
		{
			name: "a cancel with no job id still reports the cancel",
			body: `{"model_id":"qwen3.5-9b","status":"cancelled"}`,
			want: "cancelled download: model=qwen3.5-9b",
		},
		{
			name: "an id the daemon did not echo falls back to the one asked for",
			body: `{"status":"cancelled","job_id":"job_1"}`,
			want: "cancelled download: model=qwen3.5-9b job=job_1",
		},
		{
			name: "an unreadable body is reported as the cancel it returned 200 for",
			body: `not json`,
			want: "cancelled download: model=qwen3.5-9b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatModelsCancel([]byte(tt.body), "qwen3.5-9b"); got != tt.want {
				t.Errorf("formatModelsCancel() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#641): `models rm` prints the cancel line
// only when it actually stopped something. The rm path keys off this
// prefix, so a change to the "nothing to stop" wording must not silently
// start printing on every removal.
func TestFormatModelsCancel_IdleAnswerIsNotMistakenForACancel(t *testing.T) {
	got := formatModelsCancel([]byte(`{"model_id":"m","status":"not_downloading"}`), "m")
	if len(got) >= len("cancelled ") && got[:len("cancelled ")] == "cancelled " {
		t.Fatalf("the idle answer %q starts with the cancelled prefix `models rm` filters on", got)
	}
}
