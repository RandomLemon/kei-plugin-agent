package memory

import "testing"

func TestConvKey(t *testing.T) {
	cases := []struct {
		name   string
		conv   Conv
		want   string
		wantOK bool
	}{
		{"群", Conv{Type: ConvTypeGroup, Platform: "qq", BotID: "b1", ChannelID: "g1"}, "group:qq:g1", true},
		{"私聊", Conv{Type: ConvTypePrivate, Platform: "qq", BotID: "b1", PeerUserID: "u1"}, "private:qq:b1:u1", true},
		{"群缺 ChannelID", Conv{Type: ConvTypeGroup, Platform: "qq", BotID: "b1"}, "", false},
		{"群缺 Platform", Conv{Type: ConvTypeGroup, ChannelID: "g1"}, "", false},
		{"私聊缺 BotID", Conv{Type: ConvTypePrivate, Platform: "qq", PeerUserID: "u1"}, "", false},
		{"私聊缺 PeerUserID", Conv{Type: ConvTypePrivate, Platform: "qq", BotID: "b1", ChannelID: "c1"}, "", false},
		{"未知类型", Conv{Type: "channel", Platform: "qq", ChannelID: "g1"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := convKey(tc.conv)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("convKey = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestDedupeKey(t *testing.T) {
	cases := []struct {
		name   string
		rec    Record
		want   string
		wantOK bool
	}{
		{"优先 MsgID", Record{MsgID: "m1", EventID: "e1"}, "m:m1", true},
		{"仅 EventID", Record{EventID: "e1"}, "e:e1", true},
		{"都为空", Record{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dedupeKey(tc.rec)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("dedupeKey = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
