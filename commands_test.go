package main

import "testing"

func TestParseNewArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    newArgs
		wantErr bool
	}{
		{
			name: "dir only",
			args: []string{"--dir", "/tmp/proj"},
			want: newArgs{dir: "/tmp/proj"},
		},
		{
			name: "cwd is a synonym for dir",
			args: []string{"--cwd", "/tmp/proj"},
			want: newArgs{dir: "/tmp/proj"},
		},
		{
			name: "full flag set plus prompt",
			args: []string{"--server", "agent-workstation", "--dir", "~/Developer/trecs-brain", "--command", "fable", "--name", "brain", "some", "initial", "prompt"},
			want: newArgs{dir: "~/Developer/trecs-brain", name: "brain", command: "fable", server: "agent-workstation", prompt: "some initial prompt"},
		},
		{
			name: "prompt before flags still joins",
			args: []string{"hello", "--dir", "/tmp/proj", "world"},
			want: newArgs{dir: "/tmp/proj", prompt: "hello world"},
		},
		{
			name:    "missing value for flag",
			args:    []string{"--dir"},
			wantErr: true,
		},
		{
			name:    "missing value for server",
			args:    []string{"--dir", "/tmp", "--server"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"--dir", "/tmp", "--bogus"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNewArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseNewArgs(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("parseNewArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}
