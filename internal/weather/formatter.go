package weather

import (
	"fmt"
	"strings"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// FormatForecasts 格式化多筆天氣預報為 LINE 回覆文字.
func FormatForecasts(forecasts []weatherstore.WeatherForecast) string {
	var sb strings.Builder
	for i, f := range forecasts {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(formatOne(&f))
	}
	return sb.String()
}

// FormatSingleForecast 格式化單筆天氣預報.
func FormatSingleForecast(f *weatherstore.WeatherForecast) string {
	return formatOne(f)
}

func formatOne(f *weatherstore.WeatherForecast) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n天氣預報:\n", f.City, f.District)

	for _, el := range f.Elements {
		if el.Value == "" {
			continue
		}
		suffix := unitSuffix(el.ElementName)
		fmt.Fprintf(&sb, "%s : %s%s\n", el.Description, el.Value, suffix)
	}

	return sb.String()
}

func unitSuffix(elementName string) string {
	switch elementName {
	case "PoP12h", "PoP6h", "RH":
		return "%"
	case "AT", "T", "Td":
		return "℃"
	default:
		return ""
	}
}
