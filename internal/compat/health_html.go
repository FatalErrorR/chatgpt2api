package compat

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"sort"
)

// health.html.tmpl is the dashboard of api/system.py health_dashboard, unchanged.
//
//go:embed health.html.tmpl
var healthTemplateText string

var healthTemplate = template.Must(template.New("health").Parse(healthTemplateText))

type typeCount struct {
	Name  string
	Count any
}

func healthHTML(payload map[string]any) string {
	stats, _ := payload["accounts"].(map[string]any)
	byType, _ := stats["by_type"].(map[string]any)
	names := make([]string, 0, len(byType))
	for name := range byType {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]typeCount, 0, len(names))
	for _, name := range names {
		rows = append(rows, typeCount{name, byType[name]})
	}
	var out bytes.Buffer
	err := healthTemplate.Execute(&out, map[string]any{
		"Healthy": payload["healthy"], "Version": payload["version"], "Stats": stats, "ByType": rows,
	})
	if err != nil {
		return fmt.Sprintf("<pre>%s</pre>", template.HTMLEscapeString(err.Error()))
	}
	return out.String()
}
