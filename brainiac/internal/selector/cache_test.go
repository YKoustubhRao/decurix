// Tests for the prompt cache the selector sizes and counts for each
// llama-server model (--cache-ram), on the repo's own laptop.
package selector

import (
	"math"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/census"
)

// TestPromptCacheIsCountedAndYieldsToResidentModels: the brain's prompt
// cache is in its memory figure, fits the budget beside the models that stay
// loaded with it, and never pushes OCR out from beside the brain.
func TestPromptCacheIsCountedAndYieldsToResidentModels(t *testing.T) {
	p := Select(thisLaptop(), cat(t), emptyStore(t), Policy{HaveProgram: noPrograms})
	brain, ok := p.Pick(catalog.RoleBrain)
	if !ok {
		t.Fatal("no brain")
	}
	if brain.PromptCacheMB <= 0 {
		t.Fatalf("the brain has no prompt cache\n%s", render(p))
	}
	m, _ := cat(t).Model(brain.Model)
	per := catalogFacts(m, brain.Source).prefixMB()
	if n := float64(brain.PromptCacheMB) / per; math.Abs(n-math.Round(n)) > 0.01 {
		t.Fatalf("cache %d MB is not a whole number of %.0f MB prompt starts", brain.PromptCacheMB, per)
	}
	vision, _ := p.Pick(catalog.RoleVision)
	if vision.MemGB != brain.MemGB || vision.PromptCacheMB != brain.PromptCacheMB {
		t.Fatalf("picks of one model disagree: brain %.2f GB/%d MB, vision %.2f GB/%d MB", brain.MemGB, brain.PromptCacheMB, vision.MemGB, vision.PromptCacheMB)
	}
	ocr, _ := p.Pick(catalog.RoleOCR)
	if ocr.Swaps {
		t.Fatalf("the prompt cache pushed OCR out from beside the brain\n%s", render(p))
	}
	budget := p.Budgets[0].BudgetGB
	if brain.MemGB+ocr.MemGB > budget+0.01 {
		t.Fatalf("brain %.2f + ocr %.2f GB is over the %.2f GB budget", brain.MemGB, ocr.MemGB, budget)
	}
	// The cache is counted: without it the brain's figure is smaller by it.
	off := Select(thisLaptop(), cat(t), emptyStore(t), Policy{HaveProgram: noPrograms, PromptPrefixes: -1})
	b0, _ := off.Pick(catalog.RoleBrain)
	if b0.PromptCacheMB != 0 {
		t.Fatalf("PromptPrefixes -1 still gave a %d MB cache", b0.PromptCacheMB)
	}
	if d := brain.MemGB - b0.MemGB; math.Abs(d-float64(brain.PromptCacheMB)/1024) > 0.02 {
		t.Fatalf("the cache adds %.2f GB to the brain, want %.2f", d, float64(brain.PromptCacheMB)/1024)
	}
	// The hybrid model's state and checkpoints are counted too.
	f := catalogFacts(m, brain.Source)
	if want := llamaBaseGB(m, brain.Source) + (f.kvMB+f.stateMB*(1+CtxCheckpoints))/1024; math.Abs(b0.MemGB-want) > 0.01 {
		t.Fatalf("brain without cache %.2f GB, want %.2f (file, KV, state and %d checkpoints)", b0.MemGB, want, CtxCheckpoints)
	}
}

// TestPromptCacheHoldsEveryDesktopWhenThereIsRoom: with memory to spare the
// brain keeps one prompt start per desktop (3), capped by MaxCacheShare.
func TestPromptCacheHoldsEveryDesktopWhenThereIsRoom(t *testing.T) {
	cs := thisLaptop()
	cs.Pools = []census.Pool{{Name: "host", Kind: "ram", TotalGB: 40, AvailableGB: 36}}
	p := Select(cs, cat(t), emptyStore(t), Policy{HaveProgram: noPrograms})
	brain, _ := p.Pick(catalog.RoleBrain)
	if brain.Source.Runtime != catalog.RuntimeLlamaCpp {
		t.Fatalf("brain %s runs on %s, want llama.cpp for this test", brain.Model, brain.Source.Runtime)
	}
	m, _ := cat(t).Model(brain.Model)
	if got, want := brain.PromptCacheMB, int(math.Ceil(3*catalogFacts(m, brain.Source).prefixMB())); got != want {
		t.Fatalf("cache %d MB, want %d (3 prompt starts)\n%s", got, want, render(p))
	}
	if float64(brain.PromptCacheMB)/1024 > MaxCacheShare*p.Budgets[0].BudgetGB+0.01 {
		t.Fatalf("cache %d MB is over %.0f%% of the budget", brain.PromptCacheMB, MaxCacheShare*100)
	}
	embed, ok := p.Pick(catalog.RoleEmbed)
	if ok && embed.PromptCacheMB != 0 {
		t.Fatalf("the embedding model got a %d MB prompt cache", embed.PromptCacheMB)
	}
}
