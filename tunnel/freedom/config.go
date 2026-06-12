package freedom

import "github.com/p4gefau1t/trojan-go/config"

type Config struct {
	LocalHost    string             `json:"local_addr" yaml:"local-addr"`
	LocalPort    int                `json:"local_port" yaml:"local-port"`
	TCP          TCPConfig          `json:"tcp" yaml:"tcp"`
	ForwardProxy ForwardProxyConfig `json:"forward_proxy" yaml:"forward-proxy"`
}

type TCPConfig struct {
	PreferIPV4 bool `json:"prefer_ipv4" yaml:"prefer-ipv4"`
	KeepAlive  bool `json:"keep_alive" yaml:"keep-alive"`
	NoDelay    bool `json:"no_delay" yaml:"no-delay"`

	// Phase 4: explicit socket tuning
	ReadBuffer   int `json:"read_buffer" yaml:"read-buffer"`       // SO_RCVBUF bytes; 0 = OS default
	WriteBuffer  int `json:"write_buffer" yaml:"write-buffer"`     // SO_SNDBUF bytes; 0 = OS default
	KeepIdleSec  int `json:"keep_idle_sec" yaml:"keep-idle-sec"`   // TCP_KEEPIDLE; 0 = kernel default
	KeepIntvlSec int `json:"keep_intvl_sec" yaml:"keep-intvl-sec"` // TCP_KEEPINTVL; 0 = kernel default
	KeepCnt      int `json:"keep_cnt" yaml:"keep-cnt"`             // TCP_KEEPCNT; 0 = kernel default
	DialTimeout  int `json:"dial_timeout" yaml:"dial-timeout"`     // seconds; default 10
}

type ForwardProxyConfig struct {
	Enabled   bool   `json:"enabled" yaml:"enabled"`
	ProxyHost string `json:"proxy_addr" yaml:"proxy-addr"`
	ProxyPort int    `json:"proxy_port" yaml:"proxy-port"`
	Username  string `json:"username" yaml:"username"`
	Password  string `json:"password" yaml:"password"`
}

func init() {
	config.RegisterConfigCreator(Name, func() interface{} {
		return &Config{
			TCP: TCPConfig{
				PreferIPV4:   false,
				NoDelay:      true,
				KeepAlive:    true,
				DialTimeout:  10,
				KeepIdleSec:  30,
				KeepIntvlSec: 10,
				KeepCnt:      3,
			},
		}
	})
}
