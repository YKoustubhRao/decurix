package selector

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Render writes the plan for a person: the machine, the budgets, one line per
// role, what was not chosen and why (verbose), what is missing, and warnings.
func (p *Plan) Render(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "Machine: %s\n", p.Machine)
	for _, b := range p.Budgets {
		if b.Unmeasured {
			fmt.Fprintf(w, "Memory:  %s pool not measured (another machine)\n", b.Pool)
			continue
		}
		fmt.Fprintf(w, "Memory:  %s pool %.1f GB free, %.1f GB kept back, %.1f GB for models\n", b.Pool, b.AvailableGB, b.ReserveGB, b.BudgetGB)
	}
	fmt.Fprintf(w, "Disk:    %.1f GB may be used for downloads\n", p.DiskBudgetGB)
	fmt.Fprintf(w, "Speed:   %.1f GB/s of weights per token on the CPU (%s)\n\n", p.DecodeGBps, p.DecodeBasis)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tMODEL\tFROM\tMEMORY\tSPEED\tQUALITY\tDOWNLOAD")
	for _, k := range p.Picks {
		dl := "installed"
		if !k.Installed {
			dl = fmt.Sprintf("%.2f GB", k.Download)
		}
		if k.Download == 0 && !k.Installed {
			dl = "-"
		}
		where := k.Pool
		if k.OnGPU {
			where += " (GPU)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f GB %s\t%s\t%.0f\t%s\n", k.Role, k.Model, k.Source.Label(), k.MemGB, where, unitText(k.Speed, k.SpeedUnit), k.Quality, dl)
	}
	tw.Flush()
	for _, k := range p.Picks {
		line := fmt.Sprintf("  %s: %s", k.Role, k.Why)
		if len(k.Shared) > 0 {
			line += "; also does " + strings.Join(k.Shared, ", ")
		}
		fmt.Fprintln(w, line)
	}
	for _, m := range p.Missing {
		fmt.Fprintf(w, "  %s: none chosen - %s\n", m.Role, m.Why)
	}
	for _, m := range p.ByBrain {
		fmt.Fprintf(w, "  %s: the brain answers it - %s\n", m.Role, m.Why)
	}
	if p.LlamaCpp != nil {
		fmt.Fprintf(w, "\nRuntime: llama.cpp %s (%s build: %s)\n", p.LlamaCpp.Tag, p.LlamaCpp.Variant, p.LlamaCpp.Asset)
	}
	if len(p.Pip) > 0 {
		fmt.Fprintf(w, "Worker:  Python packages for %s\n", strings.Join(p.Pip, ", "))
	}
	fmt.Fprintf(w, "Download: %.2f GB in all\n", p.DownloadGB)
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "Note: %s\n", warn)
	}
	if !verbose {
		return
	}
	fmt.Fprintln(w, "\nNot chosen:")
	for _, k := range p.Picks {
		for _, r := range k.Rejected {
			fmt.Fprintf(w, "  %-7s %-28s %-48s %s\n", k.Role, r.Model, r.Source, r.Why)
		}
	}
	for _, m := range append(append([]Missing(nil), p.Missing...), p.ByBrain...) {
		for _, r := range m.Rejected {
			fmt.Fprintf(w, "  %-7s %-28s %-48s %s\n", m.Role, r.Model, r.Source, r.Why)
		}
	}
}
