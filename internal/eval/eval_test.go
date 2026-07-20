package eval

import "testing"

func TestRoutine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want Result
	}{
		{name: "ordinary reply", body: "I have organized it.", want: Pass},
		{name: "empty", body: " \n", want: Repair},
		{name: "invalid binary", body: "a\x00b", want: Hold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Routine(tt.body)
			if got != tt.want {
				t.Fatalf("Routine() = %q, want %q", got, tt.want)
			}
		})
	}
}
