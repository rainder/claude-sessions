package main

import "testing"

func TestParseServerFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    serverFlags
		wantErr bool
	}{
		{
			name: "no args uses defaults",
			args: nil,
			want: serverFlags{port: defaultServerPort, bind: "127.0.0.1"},
		},
		{
			name: "port only",
			args: []string{"--port", "9999"},
			want: serverFlags{port: 9999, bind: "127.0.0.1"},
		},
		{
			name: "bind only",
			args: []string{"--bind", "tailscale"},
			want: serverFlags{port: defaultServerPort, bind: "tailscale"},
		},
		{
			name: "both flags in either order",
			args: []string{"--bind", "0.0.0.0", "--port", "1234"},
			want: serverFlags{port: 1234, bind: "0.0.0.0"},
		},
		{
			name:    "port missing its value",
			args:    []string{"--port"},
			wantErr: true,
		},
		{
			name:    "bind missing its value",
			args:    []string{"--bind"},
			wantErr: true,
		},
		{
			name:    "port is not a number",
			args:    []string{"--port", "http"},
			wantErr: true,
		},
		{
			name:    "unknown flag is an error, not a silent no-op",
			args:    []string{"--verbose"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseServerFlags(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseServerFlags(%q) = %+v, want error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServerFlags(%q) unexpected error: %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("parseServerFlags(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}
