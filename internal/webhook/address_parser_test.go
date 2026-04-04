package webhook

import "testing"

func TestParseAddress(t *testing.T) {
	tests := []struct {
		name         string
		address      string
		wantCity     string
		wantDistrict string
	}{
		{
			name:         "台北市信義區",
			address:      "台灣台北市信義區市府路1號",
			wantCity:     "台北市",
			wantDistrict: "信義區",
		},
		{
			name:         "新北市板橋區",
			address:      "台灣新北市板橋區中山路一段161號",
			wantCity:     "新北市",
			wantDistrict: "板橋區",
		},
		{
			name:         "臺北市轉台北市",
			address:      "台灣臺北市大安區忠孝東路",
			wantCity:     "台北市",
			wantDistrict: "大安區",
		},
		{
			name:         "高雄市前鎮區",
			address:      "台灣高雄市前鎮區成功二路",
			wantCity:     "高雄市",
			wantDistrict: "前鎮區",
		},
		{
			name:         "台南縣白河鎮",
			address:      "台灣台南縣白河鎮中正路",
			wantCity:     "台南縣",
			wantDistrict: "白河鎮",
		},
		{
			name:         "新竹縣竹北市",
			address:      "台灣新竹縣竹北市光明六路",
			wantCity:     "新竹縣",
			wantDistrict: "竹北市",
		},
		{
			name:         "花蓮縣吉安鄉",
			address:      "台灣花蓮縣吉安鄉中正路",
			wantCity:     "花蓮縣",
			wantDistrict: "吉安鄉",
		},
		{
			name:         "空地址",
			address:      "",
			wantCity:     "",
			wantDistrict: "",
		},
		{
			name:         "無法解析",
			address:      "某個不是地址的字串",
			wantCity:     "",
			wantDistrict: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			city, district := ParseAddress(tt.address)
			if city != tt.wantCity {
				t.Errorf("city = %q, want %q", city, tt.wantCity)
			}
			if district != tt.wantDistrict {
				t.Errorf("district = %q, want %q", district, tt.wantDistrict)
			}
		})
	}
}
