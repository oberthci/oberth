package main

func displayPath(row command) string {
	if row.raw || row.definition.Command == "-" {
		return "tool " + row.definition.Name
	}
	return row.definition.Command
}
