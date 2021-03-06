// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package util

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/twinj/uuid"
)

//CheckAndCreateDir check and create dir
func CheckAndCreateDir(path string) error {
	if subPathExists, err := FileExists(path); err != nil {
		return fmt.Errorf("Could not determine if subPath %s exists; will not attempt to change its permissions", path)
	} else if !subPathExists {
		// Create the sub path now because if it's auto-created later when referenced, it may have an
		// incorrect ownership and mode. For example, the sub path directory must have at least g+rwx
		// when the pod specifies an fsGroup, and if the directory is not created here, Docker will
		// later auto-create it with the incorrect mode 0750
		if err := os.MkdirAll(path, 0755); err != nil {
			return fmt.Errorf("failed to mkdir:%s", path)
		}

		if err := os.Chmod(path, 0755); err != nil {
			return err
		}
	}
	return nil
}

//FileExists check file exist
func FileExists(filename string) (bool, error) {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

//CmdRunWithTimeout exec cmd with timeout
func CmdRunWithTimeout(cmd *exec.Cmd, timeout time.Duration) (bool, error) {
	done := make(chan error)
	if cmd.Process != nil { //还原执行状态
		cmd.Process = nil
		cmd.ProcessState = nil
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	go func() {
		done <- cmd.Wait()
	}()
	var err error
	select {
	case <-time.After(timeout):
		// timeout
		if err = cmd.Process.Kill(); err != nil {
			logrus.Errorf("failed to kill: %s, error: %s", cmd.Path, err.Error())
		}
		go func() {
			<-done // allow goroutine to exit
		}()
		logrus.Info("process:%s killed", cmd.Path)
		return true, err
	case err = <-done:
		return false, err
	}
}

//ReadHostID 读取当前机器ID
//ID是节点的唯一标识，acp_node将把ID与机器信息的绑定关系维护于etcd中
func ReadHostID(filePath string) (string, error) {
	if filePath == "" {
		filePath = "/etc/goodrain/host_uuid.conf"
	}
	_, err := os.Stat(filePath)
	if err != nil {
		if strings.HasSuffix(err.Error(), "no such file or directory") {
			uid := uuid.NewV4().String()
			err = ioutil.WriteFile(filePath, []byte("host_uuid="+uid), 0777)
			if err != nil {
				logrus.Error("Write host_uuid file error.", err.Error())
			}
			return uid, nil
		}
		return "", err
	}
	body, err := ioutil.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	info := strings.Split(strings.TrimSpace(string(body)), "=")
	if len(info) == 2 {
		return info[1], nil
	}
	return "", fmt.Errorf("Invalid host uuid from file")
}

//LocalIP 获取本机 ip
// 获取第一个非 loopback ip
func LocalIP() (net.IP, error) {
	tables, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		addrs, err := t.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4, nil
			}
		}
	}
	return nil, fmt.Errorf("cannot find local IP address")
}

//GetIDFromKey 从 etcd 的 key 中取 id
func GetIDFromKey(key string) string {
	index := strings.LastIndex(key, "/")
	if index < 0 {
		return ""
	}
	if strings.Contains(key, "-") { //build in任务，为了给不同node做一个区分
		return strings.Split(key[index+1:], "-")[0]
	}

	return key[index+1:]
}
