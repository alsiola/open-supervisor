package store

import "testing"

func TestSameSHA(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	if !SameSHA(full, full[:12]) || !SameSHA(full[:7], full) || SameSHA(full, "0123456789abcdef0123456789abcdef0123456X") || SameSHA("", "") || SameSHA("abc", "abcdef1") {
		t.Fatal("SameSHA")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, nil)
	s.Create("12", "Twelve", "", "sess")
	s.Update("12", func(r *Record) { r.AddPR(5); r.AddPR(5); r.HeadSHA = "abcdef1234" })
	if r := s.ByPR(5); r == nil || len(r.PRs) != 1 {
		t.Fatal("ByPR")
	}
	if s.BySHA("abcdef1234ffff") == nil {
		t.Fatal("BySHA")
	}
	again := New(dir, nil)
	if err := again.Load(); err != nil || again.Get("12") == nil || again.LastClaim("12").IsZero() {
		t.Fatal("reload", err)
	}
	again.Remove("12")
	if again.Get("12") != nil || again.LastClaim("12").IsZero() {
		t.Fatal("a removed issue keeps its claim time")
	}
}
