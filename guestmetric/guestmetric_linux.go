package guestmetric

import (
	xenstoreclient "../xenstoreclient"
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Collector struct {
	Client xenstoreclient.XenStoreClient
	Ballon bool
	Debug  bool
}

func (c *Collector) CollectOS() (GuestMetric, error) {
	current := make(GuestMetric, 0)
	f, err := os.OpenFile("/var/cache/xe-linux-distribution", os.O_RDONLY, 0666)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(strings.Trim(strings.TrimSpace(parts[1]), "\""))
			current[k] = v
		}
	}
	return prefixKeys("data/", current), nil
}

func (c *Collector) CollectMisc() (GuestMetric, error) {
	current := make(GuestMetric, 0)
	if c.Ballon {
		current["control/feature-balloon"] = "1"
	} else {
		current["control/feature-balloon"] = "0"
	}
	current["attr/PVAddons/Installed"] = "1"
	current["attr/PVAddons/MajorVersion"] = "@PRODUCT_MAJOR_VERSION@"
	current["attr/PVAddons/MinorVersion"] = "@PRODUCT_MINOR_VERSION@"
	current["attr/PVAddons/MicroVersion"] = "@PRODUCT_MICRO_VERSION@"
	current["attr/PVAddons/BuildVersion"] = "@NUMERIC_BUILD_NUMBER@"

	return current, nil
}

func (c *Collector) CollectMemory() (GuestMetric, error) {
	current := make(GuestMetric, 0)
	f, err := os.OpenFile("/proc/meminfo", os.O_RDONLY, 0666)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := regexp.MustCompile(`\w+`).FindAllString(scanner.Text(), -1)
		switch parts[0] {
		case "MemTotal":
			current["meminfo_total"] = parts[1]
		case "MemFree":
			current["meminfo_free"] = parts[1]
		}
	}
	return prefixKeys("data/", current), nil
}

func EnumNetworkAddresses(iface string) (GuestMetric, error) {
	const (
		IP_RE   string = `(\d{1,3}\.){3}\d{1,3}`
		IPV6_RE string = `[\da-f:]+[\da-f]`
		MAC_RE  string = `[\da-fA-F:]+`
	)

	var (
		IP_MAC_ADDR_RE        = regexp.MustCompile(`link\/ether\s*(` + MAC_RE + `)`)
		IP_IPV4_ADDR_RE       = regexp.MustCompile(`inet\s*(` + IP_RE + `).*\se[a-zA-Z0-9]+[\s\n]`)
		IP_IPV6_ADDR_RE       = regexp.MustCompile(`inet6\s*(` + IPV6_RE + `)`)
		IFCONFIG_IPV4_ADDR_RE = regexp.MustCompile(`inet addr:\s*(` + IP_RE + `)`)
		IFCONFIG_IPV6_ADDR_RE = regexp.MustCompile(`inet6 addr:\s*(` + IPV6_RE + `)`)
		IFCONFIG_MAC_ADDR_RE  = regexp.MustCompile(`HWaddr\s*(` + MAC_RE + `)`)
	)

	d := make(GuestMetric, 0)

	var v4re, v6re, macre *regexp.Regexp
	var out string
	var err error
	if out, err = runCmd("ip", "addr", "show", iface); err == nil {
		v4re = IP_IPV4_ADDR_RE
		v6re = IP_IPV6_ADDR_RE
		macre = IP_MAC_ADDR_RE
	} else if out, err = runCmd("ifconfig", iface); err == nil {
		v4re = IFCONFIG_IPV4_ADDR_RE
		v6re = IFCONFIG_IPV6_ADDR_RE
		macre = IFCONFIG_MAC_ADDR_RE
	} else {
		return nil, fmt.Errorf("Cannot find ip/ifconfig command")
	}

	m := v4re.FindAllStringSubmatch(out, -1)
	if m != nil {
		for _, parts := range m {
			d["ip"] = parts[1]
		}
	}
	m = v6re.FindAllStringSubmatch(out, -1)
	if m != nil {
		for i, parts := range m {
			d[fmt.Sprintf("ipv6/%d/addr", i)] = parts[1]
		}
	}

	m = macre.FindAllStringSubmatch(out, -1)
	if m != nil {
		for i, parts := range m {
			d[fmt.Sprintf("mac/%d", i)] = parts[1]
		}
	}
	return d, nil
}

func (c *Collector) CollectNetworkAddr() (GuestMetric, error) {
	current := make(GuestMetric, 0)

	paths, err := filepath.Glob("/sys/class/net/e*")
	if err != nil {
		return nil, err
	}

	for _, path := range paths {
		iface := filepath.Base(path)
		if addrs, err := EnumNetworkAddresses(iface); err == nil {
			for tag, addr := range addrs {
				current[fmt.Sprintf("%s/%s", iface, tag)] = addr
			}
		}
	}
	return prefixKeys("attr/", current), nil
}

func readSysfs(filename string) (string, error) {
	f, err := os.OpenFile(filename, os.O_RDONLY, 0666)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Scan()
	return scanner.Text(), nil
}

func (c *Collector) CollectDisk() (GuestMetric, error) {
	pi := make(GuestMetric, 0)

	disks := make([]string, 0)
	paths, err := filepath.Glob("/sys/block/*/device")
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		disk := filepath.Base(strings.TrimSuffix(filepath.Dir(path), "/"))
		disks = append(disks, disk)
	}

	var sortedDisks sort.StringSlice = disks
	sortedDisks.Sort()

	part_idx := 0
	for _, disk := range sortedDisks[:] {
		paths, err = filepath.Glob(fmt.Sprintf("/dev/%s?*", disk))
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			p := filepath.Base(path)
			line, err := readSysfs(fmt.Sprintf("/sys/block/%s/%s/size", disk, p))
			if err != nil {
				return nil, err
			}
			size, err := strconv.ParseInt(line, 10, 64)
			if err != nil {
				return nil, err
			}
			blocksize := 512
			if bs, err := readSysfs(fmt.Sprintf("/sys/block/%s/queue/physical_block_size", p)); err == nil {
				if bs1, err := strconv.Atoi(bs); err == nil {
					blocksize = bs1
				}
			}
			real_dev := ""
			if c.Client != nil {
				nodename, err := readSysfs(fmt.Sprintf("/sys/block/%s/device/nodename", disk))
				if err != nil {
					return nil, err
				}
				backend, err := c.Client.Read(fmt.Sprintf("%s/backend", nodename))
				if err != nil {
					return nil, err
				}
				real_dev, err = c.Client.Read(fmt.Sprintf("%s/dev", backend))
				if err != nil {
					return nil, err
				}
			}
			name := path
			blkid, err := runCmd("blkid", "-s", "UUID", path)
			if err != nil {
				// ignore blkid errors
				blkid = ""
			}
			if strings.Contains(blkid, "=") {
				parts := strings.SplitN(strings.TrimSpace(blkid), "=", 2)
				name = fmt.Sprintf("%s(%s)", name, strings.Trim(parts[1], "\""))
			}
			i := map[string]string{
				"extents/0": real_dev,
				"name":      name,
				"size":      strconv.FormatInt(size*int64(blocksize), 10),
			}
			output, err := runCmd("pvs", "--noheadings", "--units", "b", path)
			if err == nil && output != "" {
				parts := regexp.MustCompile(`\s+`).Split(output, -1)[1:]
				i["free"] = strings.TrimSpace(parts[5])[:len(parts[5])-1]
				i["filesystem"] = strings.TrimSpace(parts[2])
				i["mount_points/0"] = "[LVM]"
			} else {
				output, err = runCmd("mount")
				if err == nil {
					m := regexp.MustCompile(`(?m)^(\S+) on (\S+) type (\S+)`).FindAllStringSubmatch(output, -1)
					if m != nil {
						for _, parts := range m {
							if parts[1] == path {
								i["mount_points/0"] = parts[2]
								i["filesystem"] = parts[3]
								break
							}
						}
					}
				}
				output, err = runCmd("df", path)
				if err == nil {
					scanner := bufio.NewScanner(bytes.NewReader([]byte(output)))
					scanner.Scan()
					scanner.Scan()
					parts := regexp.MustCompile(`\s+`).Split(scanner.Text(), -1)
					free, err := strconv.ParseInt(parts[3], 10, 64)
					if err == nil {
						i["free"] = strconv.FormatInt(free*1024, 10)
					}
				}
			}
			for k, v := range i {
				pi[fmt.Sprintf("data/volumes/%d/%s", part_idx, k)] = v
			}
			part_idx += 1
		}
	}
	return pi, nil
}
