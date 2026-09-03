package cursor

import "strings"

const modelIDPrefix = "cursor-"

// NormalizeModelID removes Cursor's catalog-only namespace from a model ID.
func NormalizeModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if len(modelID) >= len(modelIDPrefix) && strings.EqualFold(modelID[:len(modelIDPrefix)], modelIDPrefix) {
		return strings.TrimSpace(modelID[len(modelIDPrefix):])
	}
	return modelID
}

// NormalizeModelDetails converts persisted or discovered models to public IDs.
func NormalizeModelDetails(models []ModelDetails) []ModelDetails {
	normalized := make([]ModelDetails, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		catalogID := strings.TrimSpace(model.ID)
		if catalogID == "" {
			catalogID = strings.TrimSpace(model.UpstreamID)
		}
		if strings.TrimSpace(model.UpstreamID) == "" {
			model.UpstreamID = catalogID
		} else {
			model.UpstreamID = strings.TrimSpace(model.UpstreamID)
		}
		model.ID = NormalizeModelID(catalogID)
		if model.ID == "" {
			continue
		}
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		model.DisplayModelID = NormalizeModelID(model.DisplayModelID)
		aliases := make([]string, 0, len(model.Aliases))
		aliasSeen := make(map[string]struct{}, len(model.Aliases))
		for _, alias := range model.Aliases {
			alias = NormalizeModelID(alias)
			if alias == "" || alias == model.ID {
				continue
			}
			if _, ok := aliasSeen[alias]; ok {
				continue
			}
			aliasSeen[alias] = struct{}{}
			aliases = append(aliases, alias)
		}
		model.Aliases = aliases
		normalized = append(normalized, model)
	}
	return normalized
}
