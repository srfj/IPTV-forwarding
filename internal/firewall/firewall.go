// Package firewall 尽力为应用自身端口放行系统防火墙。
//
// 飞牛 fnOS 等 NAS 系统默认启用防火墙，且只自动放行 manifest 中声明的
// service_port。本应用的流代理端口（8081/8086）与 ZeroTier 的 UDP 9993
// 需要在宿主机防火墙上显式放行，否则 ZeroTier 虚拟网段内的入站连接会被
// 静默丢弃（表现为客户端扫描超时、列表拿不到）。
package firewall

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// Open 尝试放行指定 TCP/UDP 端口，并放行 ZeroTier 虚拟网卡（zt*）的入站流量。
// 任何一步失败都只记录日志，不阻断应用启动。
func Open(tcpPorts, udpPorts []int) {
	if _, err := exec.LookPath("iptables"); err == nil {
		// ZeroTier 虚拟网卡名形如 ztxxxxxxx，用 zt+ 通配匹配
		ensure([]string{"-i", "zt+", "-j", "ACCEPT"})
		for _, p := range tcpPorts {
			if p <= 0 {
				continue
			}
			ensure([]string{"-p", "tcp", "--dport", fmt.Sprint(p), "-j", "ACCEPT"})
		}
		for _, p := range udpPorts {
			if p <= 0 {
				continue
			}
			ensure([]string{"-p", "udp", "--dport", fmt.Sprint(p), "-j", "ACCEPT"})
		}
	}

	// IPv6：ZeroTier 的成员路径可能走 IPv6，若 ip6tables 拦截 UDP 9993，
	// 会直接导致打洞/中继失败。这里一并放行（尽力而为）。
	if _, err := exec.LookPath("ip6tables"); err == nil {
		ensure6([]string{"-i", "zt+", "-j", "ACCEPT"})
		for _, p := range udpPorts {
			if p > 0 {
				ensure6([]string{"-p", "udp", "--dport", fmt.Sprint(p), "-j", "ACCEPT"})
			}
		}
		for _, p := range tcpPorts {
			if p > 0 {
				ensure6([]string{"-p", "tcp", "--dport", fmt.Sprint(p), "-j", "ACCEPT"})
			}
		}
	}

	if _, err := exec.LookPath("nft"); err == nil {
		// 部分系统（如飞牛）防火墙由 nftables 独立管理，iptables 规则不生效
		for _, tbl := range []string{"inet filter", "inet fw4", "inet firewalld"} {
			parts := strings.Fields(tbl)
			if err := exec.Command("nft", "list", "table", parts[0], parts[1]).Run(); err != nil {
				continue
			}
			_ = exec.Command("nft", "add", "rule", parts[0], parts[1], "input",
				"iifname", "zt*", "accept").Run()
			for _, p := range tcpPorts {
				if p > 0 {
					_ = exec.Command("nft", "add", "rule", parts[0], parts[1], "input",
						"tcp", "dport", fmt.Sprint(p), "accept").Run()
				}
			}
			for _, p := range udpPorts {
				if p > 0 {
					_ = exec.Command("nft", "add", "rule", parts[0], parts[1], "input",
						"udp", "dport", fmt.Sprint(p), "accept").Run()
				}
			}
		}
	}

	if _, err := exec.LookPath("ufw"); err == nil {
		for _, p := range tcpPorts {
			_ = exec.Command("ufw", "allow", fmt.Sprintf("%d/tcp", p)).Run()
		}
		for _, p := range udpPorts {
			_ = exec.Command("ufw", "allow", fmt.Sprintf("%d/udp", p)).Run()
		}
	}

	if _, err := exec.LookPath("firewall-cmd"); err == nil {
		for _, p := range tcpPorts {
			_ = exec.Command("firewall-cmd", "--permanent", "--add-port", fmt.Sprintf("%d/tcp", p)).Run()
		}
		for _, p := range udpPorts {
			_ = exec.Command("firewall-cmd", "--permanent", "--add-port", fmt.Sprintf("%d/udp", p)).Run()
		}
	}
}

// ensure 幂等地向 INPUT 链插入一条 ACCEPT 规则
func ensure(rule []string) {
	check := append([]string{"-C", "INPUT"}, rule...)
	if err := exec.Command("iptables", check...).Run(); err == nil {
		return // 规则已存在
	}
	add := append([]string{"-I", "INPUT"}, rule...)
	if err := exec.Command("iptables", add...).Run(); err != nil {
		log.Printf("[firewall] 放行失败 iptables %s: %v", strings.Join(rule, " "), err)
	}
}

// ensure6 幂等地向 ip6tables INPUT 链插入一条 ACCEPT 规则
func ensure6(rule []string) {
	check := append([]string{"-C", "INPUT"}, rule...)
	if err := exec.Command("ip6tables", check...).Run(); err == nil {
		return
	}
	add := append([]string{"-I", "INPUT"}, rule...)
	_ = exec.Command("ip6tables", add...).Run()
}
