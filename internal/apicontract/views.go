package apicontract

import "reflect"

// Views declares the HTTP-only reads alongside the tool contract.
type Views struct {
	Counts       Binding[Empty, RunCounts]         `tool:"run_counts" get:"/api/runs/counts" desc:"Read complete authorized run counts."`
	Containers   Binding[IDArgs, ContainerPage]    `tool:"run_containers" get:"/api/runs/{run}/containers" desc:"List native containers for an authorized run."`
	ContainerLog Binding[ContainerArgs, LogResult] `tool:"run_container_log" get:"/api/runs/{run}/containerlog" desc:"Read the selected authorized container log window."`
}

// ViewDefinitions uses the same typed adapter and budget rules without adding tools.
func ViewDefinitions(v Views) []Definition { return definitions(reflect.ValueOf(v)) }
