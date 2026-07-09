package main

import (
	"context"
	"math"
	"testing"
)

func TestEloWinnerGainsLoserLoses(t *testing.T) {
	// Equal ratings: the winner should gain exactly K/2 (=16) and the loser lose
	// the same, since the expected score is 0.5 each.
	na, nb := elo(1200, 1200, 1)
	if math.Abs(na-1216) > 1e-9 || math.Abs(nb-1184) > 1e-9 {
		t.Fatalf("equal-rating win: got %.4f/%.4f, want 1216/1184", na, nb)
	}
	// Zero-sum: total rating is conserved.
	if math.Abs((na+nb)-2400) > 1e-9 {
		t.Fatalf("elo not zero-sum: %.4f", na+nb)
	}
	// Draw between equals leaves ratings unchanged.
	da, db := elo(1200, 1200, 0.5)
	if math.Abs(da-1200) > 1e-9 || math.Abs(db-1200) > 1e-9 {
		t.Fatalf("equal-rating draw changed ratings: %.4f/%.4f", da, db)
	}
}

func TestMemoryRatingsRecordAndLeaderboard(t *testing.T) {
	r := NewMemoryRatings()
	ctx := context.Background()
	// alice beats bob at hive.
	if err := r.RecordResult(ctx, "hive", []string{"alice", "bob"}, matchResult{Winner: "alice"}); err != nil {
		t.Fatal(err)
	}
	lb, err := r.Leaderboard(ctx, "hive", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 2 {
		t.Fatalf("want 2 entries, got %d", len(lb))
	}
	if lb[0].Player != "alice" || lb[0].Wins != 1 || lb[0].Rating <= 1200 {
		t.Fatalf("winner row wrong: %+v", lb[0])
	}
	if lb[1].Player != "bob" || lb[1].Losses != 1 || lb[1].Rating >= 1200 {
		t.Fatalf("loser row wrong: %+v", lb[1])
	}

	// A different game keeps a separate ladder.
	if err := r.RecordResult(ctx, "eights", []string{"bob", "carol"}, matchResult{Winner: "bob"}); err != nil {
		t.Fatal(err)
	}
	hive, _ := r.Leaderboard(ctx, "hive", 10)
	if len(hive) != 2 {
		t.Fatalf("hive ladder leaked cross-game entries: %d", len(hive))
	}
	eights, _ := r.Leaderboard(ctx, "eights", 10)
	if len(eights) != 2 || eights[0].Player != "bob" {
		t.Fatalf("eights ladder wrong: %+v", eights)
	}
}

func TestRecordResultIgnoresEmptyResult(t *testing.T) {
	r := NewMemoryRatings()
	ctx := context.Background()
	if err := r.RecordResult(ctx, "hive", []string{"a", "b"}, matchResult{}); err != nil {
		t.Fatal(err)
	}
	lb, _ := r.Leaderboard(ctx, "hive", 10)
	if len(lb) != 0 {
		t.Fatalf("empty result should not create rows, got %d", len(lb))
	}
}

func TestThreePlayerNoEloJustCounts(t *testing.T) {
	r := NewMemoryRatings()
	ctx := context.Background()
	if err := r.RecordResult(ctx, "kot", []string{"a", "b", "c"}, matchResult{Winner: "a"}); err != nil {
		t.Fatal(err)
	}
	lb, _ := r.Leaderboard(ctx, "kot", 10)
	if len(lb) != 3 {
		t.Fatalf("want 3 rows, got %d", len(lb))
	}
	for _, e := range lb {
		if e.Rating != 1200 {
			t.Fatalf("3-player match should not move ELO, %s at %.2f", e.Player, e.Rating)
		}
		if e.Games != 1 {
			t.Fatalf("%s games=%d, want 1", e.Player, e.Games)
		}
	}
}
