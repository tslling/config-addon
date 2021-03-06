package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	config "github.com/Dreamacro/clash/config"
	"github.com/astaxie/beego/logs"
	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	starlark "go.starlark.net/starlark"
	"gopkg.in/yaml.v3"
)

const (
	defaultOriginConfigPath = "./.config/origin.yaml"
	defaultAddonConfigPath  = "./.config/addon.yaml"
)

var (
	errNoParamNoFile = fmt.Errorf("no parameter provided and no default config file found")
)

func main() {
	r := chi.NewRouter()
	r.Get("/config", configHandler)
	r.Get("/hello", hello)
	var port = "9999"
	if portEnv := os.Getenv("PORT"); portEnv != "" {
		port = portEnv
	}
	addr := "0.0.0.0:" + port

	done := make(chan error, 1)
	go func() {
		logs.Info("listenning on " + addr)
		err := http.ListenAndServe(addr, r)
		done <- err
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-done:
		logs.Error("%s\n", err.Error())
	case s := <-sig:
		logs.Error("terminated by signal:%s", s.String())
	}
}

func hello(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, r, render.M{"hello": "world"})
}

func configHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	params := make([]string, 3)
	paramNames := []string{"origin_url", "addon_url"}
	for i, name := range paramNames {
		params[i] = query.Get(name)
		if params[i] == "" {
			params[i] = os.Getenv(name)
		}
	}
	originURL, addonURL := params[0], params[1]

	originalConfig, err := getRawConfig(originURL, defaultOriginConfigPath)
	if err != nil {
		render.JSON(w, r, map[string]string{
			"err": fmt.Sprintf("get origin config error: %s", err.Error()),
		})
		return
	}

	addonConfig, err := getRawConfig(addonURL, defaultAddonConfigPath)
	if err != nil {
		// addon url is empty and local addon file not exist -> ignore addon file
		if err != errNoParamNoFile {
			render.JSON(w, r, map[string]string{
				"err": fmt.Sprintf("%s", err.Error()),
			})
			return
		}
	}

	bs, err := composeConfig(originalConfig, addonConfig)
	if err != nil {
		render.JSON(w, r, map[string]string{
			"err": fmt.Sprintf("%s", err.Error()),
		})
		return
	}
	render.Data(w, r, bs)
}

func getRawConfig(url string, localFile string) (*config.RawConfig, error) {
	if url != "" {
		return downloadConfig(url)
	}
	addon, err := ioutil.ReadFile(localFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoParamNoFile
		}
		return nil, err
	}
	addonConfig, err := config.UnmarshalRawConfig(addon)
	if err != nil {
		return nil, fmt.Errorf("unmarshal addon config error:%s", err.Error())
	}
	return addonConfig, nil
}

// download from url and parse to RawConfig
func downloadConfig(url string) (*config.RawConfig, error) {
	client := http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create new request error:%s", err.Error())

	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download config error:%s", err.Error())
	}
	defer resp.Body.Close()

	bs, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read resonse error:%s", err.Error())

	}
	cfg, err := config.UnmarshalRawConfig(bs)
	if err != nil {
		return nil, fmt.Errorf("parse config error:%s", err.Error())
	}
	return cfg, nil
}

func composeConfig(originalConfig *config.RawConfig, addonConfig *config.RawConfig) (bs []byte, err error) {
	if addonConfig == nil {
		return yaml.Marshal(originalConfig)
	}
	originalConfig, err = composeProxyGroup(originalConfig, addonConfig)
	if err != nil {
		return
	}
	originalConfig, err = composeRule(originalConfig, addonConfig)
	if err != nil {
		return
	}
	bs, err = yaml.Marshal(originalConfig)
	return bs, err
}

func composeProxyGroup(originalConfig *config.RawConfig, addonConfig *config.RawConfig) (*config.RawConfig, error) {
	groupName2code := make(map[string]string)
	// TODO: check group name conflicts
	for _, mapping := range addonConfig.ProxyGroup {
		name, ok := mapping["name"].(string)
		if !ok {
			continue
		}

		// TODO: hard-code condition, maybe better solution
		// since having only one proxy in a group does not make sense
		// we consider proxies[0] of such proxy group as code snippet
		// it's NOT OK that proxies[0] of such proxy group is not code
		proxies := mapping["proxies"].([]interface{})
		if len(proxies) == 1 {
			firstProxy, ok := proxies[0].(string)
			if ok {
				groupName2code[name] = firstProxy
			}
		}
	}

	// execute code snippet for groups
	groupName2proxyNames, err := parseProxyGroupsFromCode(originalConfig.Proxy, groupName2code)
	if err != nil {
		return nil, err
	}

	// alter addon group proxies
	groupName2addonMapping := make(map[string]map[string]interface{}, len(groupName2proxyNames))
	for _, mapping := range addonConfig.ProxyGroup {
		name, ok := mapping["name"].(string)
		if !ok {
			continue
		}
		proxies, exist := groupName2proxyNames[name]
		if exist {
			mapping["proxies"] = proxies
			groupName2addonMapping[name] = mapping
		}
	}

	// update existing proxy groups
	for index, mapping := range originalConfig.ProxyGroup {
		groupName, ok := mapping["name"].(string)
		if !ok {
			continue
		}
		if addonMapping, exist := groupName2addonMapping[groupName]; exist {
			originalConfig.ProxyGroup[index] = addonMapping
			delete(groupName2addonMapping, groupName)
		}
	}
	// append remaining proxy groups
	for _, mapping := range groupName2addonMapping {
		originalConfig.ProxyGroup = append(originalConfig.ProxyGroup, mapping)
	}

	return originalConfig, nil
}

func composeRule(originalConfig *config.RawConfig, addonConfig *config.RawConfig) (*config.RawConfig, error) {
	originalConfig.Rule = append(addonConfig.Rule, originalConfig.Rule...)
	return originalConfig, nil
}

func parseProxyGroupsFromCode(rawProxies []map[string]interface{}, groupName2code map[string]string) (groupName2proxyNames map[string][]string, err error) {
	// prepare variables for starlark
	values := make([]starlark.Value, 0, len(rawProxies))
	for _, p := range rawProxies {
		values = append(values, proxy2StarlarkDict(p))
	}
	list := starlark.NewList(values)
	thread := &starlark.Thread{Name: "parse groups"}
	predeclared := starlark.StringDict{
		"proxies": list,
	}

	groupName2proxyNames = make(map[string][]string, len(groupName2code))
	// execute starlark code snippets
	for groupName, code := range groupName2code {
		var proxy []string
		proxy, err = executeFilter(thread, &predeclared, &code)
		if err != nil {
			continue
		}
		groupName2proxyNames[groupName] = proxy
	}
	return groupName2proxyNames, err
}

// execute code snippet, execute names of groups filtered by user-defined function
// code snippt is supposed to include a function named "filter", and this function should return a list contains proxy names
// variables code snippet can use: "proxies"- a dict representing a Proxy
func executeFilter(thread *starlark.Thread, starlarkVariables *starlark.StringDict, code *string) ([]string, error) {
	const (
		userDefinedFunctionName = "filter"
	)
	// execute starlark code snippets
	globals, err := starlark.ExecFile(thread, "", *code, *starlarkVariables)
	if err != nil {
		return nil, fmt.Errorf("starlark execute error:%s", err.Error())
	}
	udf, exist := globals[userDefinedFunctionName]
	if !exist {
		return nil, fmt.Errorf("function '%s' not defined in code snippet", userDefinedFunctionName)
	}
	result, err := starlark.Call(thread, udf, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("execute code snippet error:%s", err.Error())
	}
	nameList, ok := result.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("returned result is not a list")
	}

	// extract result
	var (
		proxyNames []string
		value      starlark.Value
	)
	iter := nameList.Iterate()
	defer iter.Done()
	for iter.Next(&value) {
		quotedName := value.String()
		name, err := strconv.Unquote(quotedName)
		if err != nil {
			continue
		}
		proxyNames = append(proxyNames, name)
	}
	return proxyNames, nil
}

func proxy2StarlarkDict(m map[string]interface{}) *starlark.Dict {
	// TODO: more info. to dict, and give it a struct
	dict := starlark.NewDict(1)
	proxyName := m["name"].(string)
	dict.SetKey(starlark.String("name"), starlark.String(proxyName))
	return dict
}
