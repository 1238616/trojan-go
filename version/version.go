package version

import (
	"flag"
	"fmt"
	"runtime"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/constant"
	"github.com/p4gefau1t/trojan-go/option"
)

type versionOption struct {
	flag *bool
}

func (*versionOption) Name() string {
	return "version"
}

func (*versionOption) Priority() int {
	return 10
}

func (c *versionOption) Handle() error {
	if *c.flag {
		fmt.Println("Trojan-Go", constant.Version)
		fmt.Println("Go Version:", runtime.Version())
		fmt.Println("OS/Arch:", runtime.GOOS+"/"+runtime.GOARCH)
		fmt.Println("Git Commit:", constant.Commit)
		fmt.Println("")
		fmt.Println("Developed by PageFault (p4gefau1t)")
		fmt.Println("Fork maintained by 1238616 (cluster routing, monitoring dashboard, performance fixes)")
		fmt.Println("Licensed under GNU General Public License version 3")
		fmt.Println("GitHub Repository:\thttps://github.com/1238616/trojan-go")
		fmt.Println("Upstream Project:\thttps://github.com/p4gefau1t/trojan-go")
		fmt.Println("Trojan-Go Documents:\thttps://p4gefau1t.github.io/trojan-go/")
		return nil
	}
	return common.NewError("not set")
}

func init() {
	option.RegisterHandler(&versionOption{
		flag: flag.Bool("version", false, "Display version and help info"),
	})
}
