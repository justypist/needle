// Package proto defines the wire format between needle-client and
// needle-server. It is the only contract the two share.
package proto

// Host identifies a reporting machine. It rides along with every report so
// the server's metadata stays fresh without a separate registration step.
type Host struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	CPUModel string `json:"cpu_model"`
	Cores    int    `json:"cores"`
	BootTime int64  `json:"boot_time"`
	Agent    string `json:"agent"`
}

// Sample holds the scalar gauges for one instant. Byte counters are already
// expressed as per-second rates, so every field is a gauge and rolling a tier
// up is a plain average.
type Sample struct {
	TS        int64    `json:"ts"`
	CPU       float64  `json:"cpu"`
	MemTotal  uint64   `json:"mem_total"`
	MemUsed   uint64   `json:"mem_used"`
	MemCached uint64   `json:"mem_cached"`
	SwapTotal uint64   `json:"swap_total"`
	SwapUsed  uint64   `json:"swap_used"`
	Load1     float64  `json:"load1"`
	Load5     float64  `json:"load5"`
	Load15    float64  `json:"load15"`
	Uptime    uint64   `json:"uptime"`
	Procs     int      `json:"procs"`
	NetRx     float64  `json:"net_rx"`
	NetTx     float64  `json:"net_tx"`
	NetRxTot  uint64   `json:"net_rx_total"`
	NetTxTot  uint64   `json:"net_tx_total"`
	DiskRead  float64  `json:"disk_read"`
	DiskWrite float64  `json:"disk_write"`
	DiskRTot  uint64   `json:"disk_read_total"`
	DiskWTot  uint64   `json:"disk_write_total"`
	Temp      *float64 `json:"temp,omitempty"`
}

// Series is one labelled time series carried alongside the sample. Kind
// selects the meaning of A and B:
//
//	cpu   per-core usage percent                  (A)
//	net   per-interface rx/tx bytes per second    (A, B)
//	disk  per-device read/write bytes per second  (A, B)
//	fs    per-mount used/total bytes              (A, B)
//	temp  per-zone degrees Celsius                (A)
type Series struct {
	Kind  string  `json:"kind"`
	Label string  `json:"label"`
	A     float64 `json:"a"`
	B     float64 `json:"b,omitempty"`
}

// Report is the body of a single POST /report.
type Report struct {
	Host     Host     `json:"host"`
	Interval int      `json:"interval"`
	Sample   Sample   `json:"sample"`
	Series   []Series `json:"series,omitempty"`
}
