package apicontract

// Bindings is the single typed tool table, grouped by the service responsibility.
// Each leaf declares its input, result, guidance, authority, budgets and handler.
type Bindings struct {
	RunsTools
	IssuesTools
	AccessTools
	PromotionTools
	AdminTools
	EventsTools
}
