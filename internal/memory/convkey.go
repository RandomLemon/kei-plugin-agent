package memory

// convKey 推导 conv_key：group:<platform>:<channel_id>、private:<platform>:<bot_id>:<user_id>。
// 会话类型未知或缺少组成字段时 ok 为 false。
func convKey(c Conv) (key string, ok bool) {
	switch c.Type {
	case ConvTypeGroup:
		if c.Platform == "" || c.ChannelID == "" {
			return "", false
		}
		return "group:" + c.Platform + ":" + c.ChannelID, true
	case ConvTypePrivate:
		if c.Platform == "" || c.BotID == "" || c.PeerUserID == "" {
			return "", false
		}
		return "private:" + c.Platform + ":" + c.BotID + ":" + c.PeerUserID, true
	default:
		return "", false
	}
}

// dedupeKey 返回消息去重键：MsgID 非空时为 m:<MsgID>，否则 e:<EventID>；两者都空时 ok 为 false。
func dedupeKey(r Record) (key string, ok bool) {
	switch {
	case r.MsgID != "":
		return "m:" + r.MsgID, true
	case r.EventID != "":
		return "e:" + r.EventID, true
	default:
		return "", false
	}
}
