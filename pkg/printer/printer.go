package printer

import (
	"fmt"
	"os"
	"text/tabwriter"
)

func PodsToTable(title string, pods map[string]string) {
	fmt.Printf("\n%s:\n", title)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.Debug)
	fmt.Fprintln(w, "Pod Name\tStatus")
	fmt.Fprintln(w, "--------\t------")
	for name, status := range pods {
		fmt.Fprintf(w, "%s\t%s\n", name, status)
	}
	w.Flush()
}
