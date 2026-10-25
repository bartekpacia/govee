package main

import "testing"

func TestMatchesLamp(t *testing.T) {
	table := lamp{name: "table", addr: "5C:E7:53:C7:2D:2F"}
	tests := []struct {
		name string
		id   advID
		want bool
	}{
		{name: "linux MAC", id: advID{address: "5C:E7:53:C7:2D:2F"}, want: true},
		{name: "linux MAC lowercase", id: advID{address: "5c:e7:53:c7:2d:2f"}, want: true},
		{
			name: "macOS name",
			id:   advID{address: "02DE1200-EFCF-20DF-C45F-68E0F3B3D315", name: "Govee_H6006_2D2F"},
			want: true,
		},
		{
			name: "macOS name without an underscore",
			id:   advID{address: "F97AFFF2-CD9C-A238-C0D6-034DFFC44440", name: "GVH60062D2F"},
			want: true,
		},
		{
			name: "macOS name lowercase suffix",
			id:   advID{address: "02DE1200-EFCF-20DF-C45F-68E0F3B3D315", name: "ihoment_H6006_2d2f"},
			want: true,
		},
		{
			name: "other suffix",
			id:   advID{address: "02DE1200-EFCF-20DF-C45F-68E0F3B3D315", name: "Govee_H6006_3A37"},
			want: false,
		},
		{
			name: "name without a Govee prefix",
			id:   advID{address: "02DE1200-EFCF-20DF-C45F-68E0F3B3D315", name: "Lamp_2D2F"},
			want: false,
		},
		{
			name: "empty name on macOS",
			id:   advID{address: "02DE1200-EFCF-20DF-C45F-68E0F3B3D315"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesLamp(table, tt.id); got != tt.want {
				t.Errorf("matchesLamp() = %v, want %v", got, tt.want)
			}
		})
	}

	uuidLamp := lamp{name: "x", addr: "02DE1200-EFCF-20DF-C45F-68E0F3B3D315"}
	if !matchesLamp(uuidLamp, advID{address: "02DE1200-EFCF-20DF-C45F-68E0F3B3D315"}) {
		t.Error("a UUID lamp should match its CoreBluetooth address with no advertised name")
	}
}

func TestLampLabel(t *testing.T) {
	if got := lampLabel("5C:E7:53:C7:2D:2F", ""); got != "table" {
		t.Errorf("MAC label = %q, want table", got)
	}
	if got := lampLabel("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", "Govee_H6006_2D2F"); got != "table" {
		t.Errorf("name label = %q, want table", got)
	}
	if got := lampLabel("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", "Govee_H607C_93B7"); got != "-" {
		t.Errorf("unknown label = %q, want -", got)
	}
}

func TestMacSuffix(t *testing.T) {
	if got := macSuffix("5C:E7:53:C7:2D:2F"); got != "2D2F" {
		t.Errorf("macSuffix() = %q, want 2D2F", got)
	}
	if got := macSuffix("02DE1200-EFCF-20DF-C45F-68E0F3B3D315"); got != "" {
		t.Errorf("macSuffix(uuid) = %q, want empty", got)
	}
}
