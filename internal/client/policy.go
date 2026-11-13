package client

import "time"

type PolicyReq struct {
	EventName  string
	Host       string
	PoolStatus *PoolStats
	NowTime    time.Time
}

func NewTargetReq(target Target, poolStatus *PoolStats) *PolicyReq {
	if poolStatus == nil {
		poolStatus = &PoolStats{}
	}
	return &PolicyReq{
		EventName:  "event_target_check",
		Host:       target.Host,
		PoolStatus: poolStatus,
		NowTime:    time.Now(),
	}
}

func (r *PolicyReq) GetEvent() string {
	return r.EventName
}

func (r *PolicyReq) GetGrayCount() int {
	return 0
}
