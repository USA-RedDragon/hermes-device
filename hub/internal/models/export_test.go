package models

// Row is the part of a provider listing the status check reads.
type Row struct {
	Slug          string
	Authenticated *bool
	IsUserDefined bool
	IsCurrent     bool
}

// Ready reports whether status finds a usable model for the given choice.
func Ready(model, provider string, rows ...Row) bool {
	o := options{Model: model, Provider: provider}
	for _, r := range rows {
		o.Providers = append(o.Providers, optionsRow{Slug: r.Slug, Authenticated: r.Authenticated, IsUserDefined: r.IsUserDefined, IsCurrent: r.IsCurrent})
	}
	return status(o).Ready
}
