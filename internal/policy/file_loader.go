package policy

import (
	"os"
	"path/filepath"

	"github.com/LiZeC123/gks/internal/log"
	thoth "github.com/expr-thoth/expr-thoth"
	"github.com/expr-thoth/expr-thoth/router"
)

func LoadFileSystemPolicys(ins *thoth.Thoth, policyRoot string) bool {

	entries, err := os.ReadDir(policyRoot)
	if err != nil {
		logger.Error("加载策略根目录失败", log.Event, "load_policy_dir_fail", "err", err)
		return false
	}

	routers := make([]router.RouteConfig, 0)

	for _, entry := range entries {
		// 跳过非目录项
		if !entry.IsDir() {
			continue
		}

		// 注册该分组的路由
		dirName := entry.Name()
		routers = append(routers, router.RouteConfig{
			EventList: []string{dirName},
			GroupList: []string{dirName},
			Enabled:   true,
			Priority:  100,
		})

		dirPath := filepath.Join(policyRoot, dirName)
		subEntries, err := os.ReadDir(dirPath)
		if err != nil {
			logger.Error("加载策略分组失败", log.Event, "load_policy_group_fail", "err", err)
			continue
		}

		group := make([]*thoth.PolicyFile, 0)
		for _, sub := range subEntries {
			if sub.IsDir() {
				continue
			}

			fullName := filepath.Join(dirPath, sub.Name())
			policyFile := LoadPolicyFile(fullName)
			group = append(group, policyFile)
		}
		ins.SetGroup(dirName, group)
	}
	ins.RegisterRouteMap(routers)

	return true
}

func LoadPolicyFile(name string) *thoth.PolicyFile {
	yamlStr, err := os.ReadFile(name)
	if err != nil {
		panic("Failed to read YAML file: " + err.Error())
	}
	policyFile, err := thoth.NewPolicyFile(string(yamlStr))
	if err != nil {
		panic("Failed to load YAML: " + err.Error())
	}

	return policyFile
}
