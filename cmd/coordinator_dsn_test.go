package cmd

import "testing"

// TestResolveCoordinatorDSN locks the precedence used by the K8s/Helm
// deployment path (REQ-147 / issue #335). The chart sets
// WORKBUDDY_MYSQL_DSN; the coordinator pod must pick it up and route
// store.New to the MySQL backend rather than falling back to SQLite.
func TestResolveCoordinatorDSN(t *testing.T) {
	cases := []struct {
		name   string
		dbFlag string
		envDSN string
		want   string
	}{
		{
			name:   "bare path, no env → SQLite default",
			dbFlag: ".workbuddy/workbuddy.db",
			envDSN: "",
			want:   ".workbuddy/workbuddy.db",
		},
		{
			name:   "bare path + mysql env → MySQL DSN with prefix",
			dbFlag: ".workbuddy/workbuddy.db",
			envDSN: "user:pass@tcp(mysql:3306)/workbuddy?parseTime=true",
			want:   "mysql://user:pass@tcp(mysql:3306)/workbuddy?parseTime=true",
		},
		{
			name:   "bare path + already-prefixed env passes through",
			dbFlag: ".workbuddy/workbuddy.db",
			envDSN: "mysql://user:pass@tcp(mysql:3306)/workbuddy",
			want:   "mysql://user:pass@tcp(mysql:3306)/workbuddy",
		},
		{
			name:   "flag wins when it carries a scheme",
			dbFlag: "mysql://flag:flag@tcp(other:3306)/db",
			envDSN: "ignored@tcp(env:3306)/db",
			want:   "mysql://flag:flag@tcp(other:3306)/db",
		},
		{
			name:   "explicit sqlite:// flag wins over env",
			dbFlag: "sqlite:///tmp/x.db",
			envDSN: "user@tcp(mysql:3306)/db",
			want:   "sqlite:///tmp/x.db",
		},
		{
			name:   "whitespace is trimmed",
			dbFlag: "  .workbuddy/workbuddy.db  ",
			envDSN: "   user@tcp(mysql:3306)/db   ",
			want:   "mysql://user@tcp(mysql:3306)/db",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCoordinatorDSN(tc.dbFlag, tc.envDSN)
			if got != tc.want {
				t.Fatalf("resolveCoordinatorDSN(%q, %q) = %q, want %q",
					tc.dbFlag, tc.envDSN, got, tc.want)
			}
		})
	}
}

// TestResolveCoordinatorDSN_DispatchSelectsMySQL is an indirect check that
// the resolved DSN actually carries the mysql:// scheme that store.New
// uses to dispatch — guards against a regression where the env is
// honored but the prefix gets dropped before reaching store.newWithMode.
func TestResolveCoordinatorDSN_DispatchSelectsMySQL(t *testing.T) {
	t.Setenv("WORKBUDDY_MYSQL_DSN", "user:pass@tcp(mysql.svc:3306)/workbuddy?parseTime=true")
	got := resolveCoordinatorDSN(".workbuddy/workbuddy.db", "user:pass@tcp(mysql.svc:3306)/workbuddy?parseTime=true")
	if got == "" || got[:8] != "mysql://" {
		t.Fatalf("expected mysql:// prefix so store.New routes to MySQL dialect, got %q", got)
	}
}
