package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/mxiao/todo-cli/packages/agent"
	"github.com/mxiao/todo-cli/packages/llm"
	"github.com/mxiao/todo-cli/packages/prompt"
)

// LLMHooks replace the model plumbing, so tests run against mock models;
// nil fields use the real HTTP client, macOS keychain and processes.
type LLMHooks struct {
	NewClient func(*llm.Resolved) (llm.Client, error)
	Secrets   llm.SecretStore
	Runner    llm.Runner
}

func (a *app) secretStore() llm.SecretStore {
	if a.env.LLM.Secrets != nil {
		return a.env.LLM.Secrets
	}
	return llm.NewKeychain()
}

// llm opens the model service over the task store; model-made changes are
// recorded with history actor "llm".
func (a *app) llm() (*llm.Service, error) {
	if a.llmSvc != nil {
		return a.llmSvc, nil
	}
	s, err := a.open()
	if err != nil {
		return nil, err
	}
	origin := a.actor
	if origin == "" {
		origin = "cli"
	}
	svc, err := llm.Open(s, llm.Options{Getenv: a.env.Getenv, Secrets: a.secretStore(), Runner: a.env.LLM.Runner,
		NewClient: a.env.LLM.NewClient, Now: a.env.Now, Origin: origin})
	if err != nil {
		return nil, err
	}
	a.llmSvc = svc
	// Accepted agent items of model decisions start agent runs.
	if lib, err := prompt.Open(s); err == nil {
		lib.Redact = svc.Redact
		if m, err := agent.Open(s, agent.Options{LLM: svc, Prompts: lib, Origin: origin, Getenv: a.env.Getenv}); err == nil {
			svc.SetAgentLauncher(m)
			a.agentMgr = m
		} else {
			a.agentErr = err
		}
	}
	return svc, nil
}

// ctx is cancelled by Ctrl+C so long model calls can be interrupted.
func (a *app) ctx() (context.Context, context.CancelFunc) {
	if a.env.Ctx != nil {
		return context.WithCancel(a.env.Ctx)
	}
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// subcommand is one verb of a command group (todo llm …, todo ai …).
type subcommand struct {
	name    string
	args    string
	summary string
	run     func(a *app, args []string) error
}

// runGroup dispatches to a subcommand; no arguments run def.
func (a *app) runGroup(group string, subs []subcommand, def string, args []string) error {
	if len(args) == 0 {
		if def == "" {
			return a.groupHelp(group, subs)
		}
		args = []string{def}
	}
	switch args[0] {
	case "-h", "--help", "help":
		return a.groupHelp(group, subs)
	}
	for _, s := range subs {
		if s.name == args[0] {
			return s.run(a, args[1:])
		}
	}
	var names []string
	for _, s := range subs {
		names = append(names, s.name)
	}
	return usagef("todo %s: unknown subcommand %q (want %s)", group, args[0], strings.Join(names, ", "))
}

func (a *app) groupHelp(group string, subs []subcommand) error {
	c := findCommand(group)
	if c != nil {
		a.printf("usage: todo %s <subcommand>\n\n%s\n\nsubcommands:\n", c.name, c.summary)
	}
	for _, s := range subs {
		a.printf("  %-44s %s\n", s.name+" "+s.args, s.summary)
	}
	a.printf("\nRun `todo %s <subcommand> -h` for flags.\n", group)
	return errHelpShown
}

// subFlags parses a subcommand's flags; -h prints its usage.
func (a *app) subFlags(group, name, args string, fs func(*fset), in []string) ([]string, error) {
	f := newFlags(group + " " + name)
	if fs != nil {
		fs(&fset{f})
	}
	if slices.ContainsFunc(in, func(s string) bool { return s == "-h" || s == "--help" || s == "-help" }) {
		a.printf("usage: todo %s %s %s\n", group, name, args)
	}
	return a.parse(f, in)
}

// flagSet adds list flags to flag.FlagSet.
type fset struct{ *flag.FlagSet }

// List registers a repeatable, comma-separated flag.
func (f *fset) List(name, usage string) *stringList {
	p := new(stringList)
	f.Var(p, name, usage)
	return p
}

// ---- todo llm ----

func cmdLLM(a *app, args []string) error {
	return a.runGroup("llm", []subcommand{
		{"status", "", "Show the model configuration, permission mode and confirm list", llmStatus},
		{"test", "[--profile P]", "Test the connection to the model service", llmTest},
		{"set", "[--profile P] [--provider X] [--model M] [--base-url U] [--key-env VAR] [--no-key] [--use]", "Add or change a model profile (never the key)", llmSet},
		{"use", "<profile>", "Switch the active model profile", llmUse},
		{"remove", "<profile>", "Remove a model profile", llmRemove},
		{"set-key", "[--profile P]", "Store the API key in the macOS keychain (read from stdin)", llmSetKey},
		{"delete-key", "[--profile P]", "Remove the API key from the macOS keychain", llmDeleteKey},
		{"mode", "[suggest|confirm|auto]", "Show or set the permission mode (仅建议/执行前确认/自动执行)", llmMode},
		{"confirm", "[list|add|remove] [entry]", "Manage the custom confirm list (categories, kinds or command prefixes)", llmConfirm},
		{"commands", "[on|off]", "Allow or forbid model-proposed commands", llmCommands},
		{"fields", "[--send a,b] [--decision c,d]", "Show or set the task fields sent to the model", llmFields},
		{"agent", "[list|add <name> -- <cmd> [args]|remove <name>]", "Manage the command-line agents the model may start", llmAgent},
		{"calls", "[--limit N]", "Show the audited model calls", llmCalls},
		{"actions", "[--limit N]", "Show the commands and agents the model ran", llmActions},
	}, "status", args)
}

func llmStatus(a *app, args []string) error {
	if _, err := a.subFlags("llm", "status", "", nil, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	st := svc.Status()
	if a.json {
		return a.emitJSON(st)
	}
	m := st.Model
	avail := "可用"
	if !st.Available {
		avail = "不可用（普通任务管理不受影响）"
	}
	a.printf("模型：      %s/%s @ %s [%s] — %s\n", m.Provider, m.Model, m.BaseURL, m.Profile, avail)
	a.printf("凭据：      %s\n", m.KeySource)
	if m.Problem != "" {
		a.printf("问题：      %s\n", m.Problem)
	}
	if st.Error != "" {
		a.printf("错误：      %s\n", st.Error)
	}
	a.printf("权限模式：  %s (%s)\n", st.ModeLabel, st.Mode)
	a.printf("默认确认：  %s\n", strings.Join(st.DefaultConfirm, ", "))
	a.printf("确认清单：  %s\n", orNone(strings.Join(st.ConfirmList, ", ")))
	a.printf("发送字段：  %s；决策另加 %s\n", strings.Join(st.SendFields, ", "), strings.Join(st.DecisionFields, ", "))
	a.printf("命令调用：  %s；智能体：%s\n", onOff(st.AllowCommands), orNone(strings.Join(st.Agents, ", ")))
	a.printf("助手配置：  v%d\n", st.ConfigVersion)
	var names []string
	for _, p := range st.Profiles {
		mark := " "
		if p.Profile == m.Profile {
			mark = "*"
		}
		names = append(names, fmt.Sprintf("%s%s (%s/%s, %s)", mark, p.Profile, p.Provider, p.Model, p.Status))
	}
	a.printf("模型配置：  %s\n", strings.Join(names, "  "))
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "（无）"
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "开启"
	}
	return "关闭"
}

func llmTest(a *app, args []string) error {
	var profile *string
	if _, err := a.subFlags("llm", "test", "[--profile P]", func(f *fset) { profile = f.String("profile", "", "profile to test (default: active)") }, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	rep, err := svc.TestConnection(ctx, *profile)
	if err != nil {
		return err
	}
	if a.json {
		if err := a.emitJSON(rep); err != nil {
			return err
		}
	} else if rep.OK {
		a.printf("连接成功：%s/%s @ %s（%d ms）\n", rep.Model.Provider, rep.Model.Model, rep.Model.BaseURL, rep.LatencyMS)
	} else {
		a.printf("连接失败 [%s]：%s\n", rep.Error.Kind, rep.Error.Message)
		if rep.Error.Hint != "" {
			a.printf("建议：%s\n", rep.Error.Hint)
		}
		if rep.Error.Retryable {
			a.printf("可重试：todo llm test\n")
		}
		if len(rep.Alternatives) > 0 {
			a.printf("可切换模型配置：%s（todo llm use <配置名>）\n", strings.Join(rep.Alternatives, ", "))
		}
	}
	if !rep.OK {
		return statusFailed{rep.Error}
	}
	return nil
}

func llmSet(a *app, args []string) error {
	var name, provider, model, baseURL, keyEnv *string
	var noKey, use *bool
	var timeout *int
	pos, err := a.subFlags("llm", "set", "", func(f *fset) {
		name = f.String("profile", "", "profile name (default: active profile)")
		provider = f.String("provider", "", "openai, deepseek, openrouter, moonshot, dashscope, ollama, lmstudio or any OpenAI-compatible name")
		model = f.String("model", "", "model name (default "+llm.DefaultModel+")")
		baseURL = f.String("base-url", "", "OpenAI-compatible endpoint, e.g. https://api.openai.com/v1")
		keyEnv = f.String("key-env", "", "environment variable holding the key (default "+llm.EnvAPIKey+")")
		noKey = f.Bool("no-key", false, "the service needs no key (local models)")
		use = f.Bool("use", false, "make this the active profile")
		timeout = f.Int("timeout", 0, "request timeout in seconds")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("llm set takes flags only (the key is set with `todo llm set-key`)")
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	st, err := svc.UpdateSettings(func(s *llm.Settings) error {
		n := *name
		if n == "" {
			n = s.Active
		}
		p := llm.Profile{Name: n}
		if cur, err := s.Profile(n); err == nil {
			p = *cur
		}
		if *provider != "" {
			if p.Provider != *provider && *baseURL == "" {
				p.BaseURL = "" // take the new provider's endpoint
			}
			p.Provider = *provider
		}
		if *model != "" {
			p.Model = *model
		}
		if *baseURL != "" {
			p.BaseURL = *baseURL
		}
		if *keyEnv != "" {
			p.APIKeyEnv = *keyEnv
		}
		if *noKey {
			p.NoKey = true
		}
		if *timeout > 0 {
			p.TimeoutSeconds = *timeout
		}
		if err := s.PutProfile(p); err != nil {
			return err
		}
		if *use {
			s.Active = p.Name
		}
		return nil
	})
	if err != nil {
		return err
	}
	return a.printSettingsDone(svc, st, "已保存模型配置")
}

func (a *app) printSettingsDone(svc *llm.Service, _ llm.Settings, msg string) error {
	if a.json {
		return a.emitJSON(svc.Status())
	}
	a.printf("%s\n", msg)
	return llmStatus(a, nil)
}

func llmUse(a *app, args []string) error {
	pos, err := a.subFlags("llm", "use", "<profile>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo llm use <profile>")
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	st, err := svc.UpdateSettings(func(s *llm.Settings) error {
		if _, err := s.Profile(pos[0]); err != nil {
			return err
		}
		s.Active = pos[0]
		return nil
	})
	if err != nil {
		return err
	}
	return a.printSettingsDone(svc, st, "已切换到模型配置 "+pos[0])
}

func llmRemove(a *app, args []string) error {
	pos, err := a.subFlags("llm", "remove", "<profile>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo llm remove <profile>")
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	st, err := svc.UpdateSettings(func(s *llm.Settings) error { return s.RemoveProfile(pos[0]) })
	if err != nil {
		return err
	}
	return a.printSettingsDone(svc, st, "已删除模型配置 "+pos[0])
}

// llmSetKey reads the key from standard input so it never appears in the
// shell history or process list, and stores it in the keychain.
func llmSetKey(a *app, args []string) error {
	var name *string
	if _, err := a.subFlags("llm", "set-key", "[--profile P]", func(f *fset) { name = f.String("profile", "", "profile (default: active)") }, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	st, err := svc.Settings()
	if err != nil {
		return err
	}
	n := *name
	if n == "" {
		n = st.Active
	}
	if _, err := st.Profile(n); err != nil {
		return err
	}
	if !a.json {
		fmt.Fprintf(a.env.Stderr, "输入 %s 的 API Key 后回车（不会回显到日志）：", n)
	}
	key, err := bufio.NewReader(a.env.Stdin).ReadString('\n')
	key = strings.TrimSpace(key)
	if key == "" {
		if err != nil {
			return usagef("llm set-key: no key on standard input")
		}
		return usagef("llm set-key: empty key")
	}
	if !a.json {
		fmt.Fprintln(a.env.Stderr)
	}
	if err := a.secretStore().Set(n, key); err != nil {
		return fmt.Errorf("store the key in the keychain: %s", llm.Redact(err.Error(), key))
	}
	st, err = svc.UpdateSettings(func(s *llm.Settings) error {
		p, err := s.Profile(n)
		if err != nil {
			return err
		}
		p.Keychain = true
		return nil
	})
	if err != nil {
		return err
	}
	return a.printSettingsDone(svc, st, "API Key 已保存到 macOS 钥匙串（"+llm.KeychainService+"/"+n+"）")
}

func llmDeleteKey(a *app, args []string) error {
	var name *string
	if _, err := a.subFlags("llm", "delete-key", "[--profile P]", func(f *fset) { name = f.String("profile", "", "profile (default: active)") }, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	var n string
	st, err := svc.UpdateSettings(func(s *llm.Settings) error {
		n = *name
		if n == "" {
			n = s.Active
		}
		p, err := s.Profile(n)
		if err != nil {
			return err
		}
		p.Keychain = false
		return nil
	})
	if err != nil {
		return err
	}
	if err := a.secretStore().Delete(n); err != nil && !errors.Is(err, llm.ErrSecretNotFound) {
		return err
	}
	return a.printSettingsDone(svc, st, "已从钥匙串删除 "+n+" 的 API Key")
}

func llmMode(a *app, args []string) error {
	pos, err := a.subFlags("llm", "mode", "[suggest|confirm|auto]", nil, args)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		st, err := svc.Settings()
		if err != nil {
			return err
		}
		if a.json {
			return a.emitJSON(map[string]any{"mode": st.Mode, "label": st.Mode.Label()})
		}
		a.printf("%s (%s)\n", st.Mode.Label(), st.Mode)
		return nil
	}
	m, err := llm.ParseMode(strings.Join(pos, " "))
	if err != nil {
		return err
	}
	st, err := svc.UpdateSettings(func(s *llm.Settings) error { s.Mode = m; return nil })
	if err != nil {
		return err
	}
	return a.printSettingsDone(svc, st, "权限模式："+m.Label())
}

func llmConfirm(a *app, args []string) error {
	pos, err := a.subFlags("llm", "confirm", "[list|add|remove] [entry]", nil, args)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	verb := "list"
	if len(pos) > 0 {
		verb, pos = pos[0], pos[1:]
	}
	if verb != "list" {
		if len(pos) == 0 {
			return usagef("usage: todo llm confirm %s <entry>  (e.g. delete, command, agent.start, \"git push\")", verb)
		}
		entry, err := llm.ValidateConfirmEntry(strings.Join(pos, " "))
		if err != nil {
			return err
		}
		_, err = svc.UpdateSettings(func(s *llm.Settings) error {
			switch verb {
			case "add":
				if !slices.Contains(s.ConfirmList, entry) {
					s.ConfirmList = append(s.ConfirmList, entry)
				}
			case "remove", "rm":
				if !slices.Contains(s.ConfirmList, entry) {
					return fmt.Errorf("%w: %q is not on the confirm list", llm.ErrInvalid, entry)
				}
				s.ConfirmList = slices.DeleteFunc(s.ConfirmList, func(e string) bool { return e == entry })
			default:
				return usagef("todo llm confirm: unknown action %q (list, add, remove)", verb)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	st, err := svc.Settings()
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"confirm_list": st.ConfirmList, "default_confirm": llm.DefaultConfirm})
	}
	a.printf("默认确认（删除/覆盖/外发/提交/系统配置）：%s\n自定义确认清单：%s\n", strings.Join(llm.DefaultConfirm, ", "), orNone(strings.Join(st.ConfirmList, ", ")))
	return nil
}

func llmCommands(a *app, args []string) error {
	pos, err := a.subFlags("llm", "commands", "[on|off]", nil, args)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		var on bool
		switch pos[0] {
		case "on", "true", "yes":
			on = true
		case "off", "false", "no":
		default:
			return usagef("usage: todo llm commands on|off")
		}
		if _, err := svc.UpdateSettings(func(s *llm.Settings) error { s.AllowCommands = on; return nil }); err != nil {
			return err
		}
	}
	st, err := svc.Settings()
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"allow_commands": st.AllowCommands})
	}
	a.printf("模型命令调用：%s\n", onOff(st.AllowCommands))
	return nil
}

func llmFields(a *app, args []string) error {
	var send, decision *stringList
	if _, err := a.subFlags("llm", "fields", "", func(f *fset) {
		send = f.List("send", "fields sent for every model feature ("+strings.Join(llm.SendableFields, ", ")+")")
		decision = f.List("decision", "extra fields sent for decision support")
	}, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	if len(*send) > 0 || len(*decision) > 0 {
		if _, err := svc.UpdateSettings(func(s *llm.Settings) error {
			if len(*send) > 0 {
				s.SendFields = *send
			}
			if len(*decision) > 0 {
				s.DecisionFields = *decision
			}
			return nil
		}); err != nil {
			return err
		}
	}
	st, err := svc.Settings()
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"send_fields": st.SendFields, "decision_fields": st.DecisionFields})
	}
	a.printf("发送字段：%s\n决策另加：%s\n", strings.Join(st.SendFields, ", "), strings.Join(st.DecisionFields, ", "))
	return nil
}

func llmAgent(a *app, args []string) error {
	var dir, desc *string
	pos, err := a.subFlags("llm", "agent", "[list|add <name> -- <cmd> [args]|remove <name>]", func(f *fset) {
		dir = f.String("dir", "", "working directory")
		desc = f.String("desc", "", "what the agent is good at (shown to the model)")
	}, args)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	verb := "list"
	if len(pos) > 0 {
		verb, pos = pos[0], pos[1:]
	}
	switch verb {
	case "list":
	case "add":
		if len(pos) < 2 {
			return usagef("usage: todo llm agent add <name> [--desc D] [--dir DIR] -- <command> [args...]")
		}
		// These agents read the task prompt as plain text on stdin and
		// answer in plain text (`todo agent add` offers every option).
		ag := llm.AgentProfile{Name: pos[0], Command: pos[1:], Dir: *dir, Description: *desc, Input: "prompt-stdin", Output: "text"}
		if _, err := svc.UpdateSettings(func(s *llm.Settings) error {
			s.Agents = slices.DeleteFunc(s.Agents, func(x llm.AgentProfile) bool { return x.Name == ag.Name })
			s.Agents = append(s.Agents, ag)
			return nil
		}); err != nil {
			return err
		}
	case "remove", "rm":
		if len(pos) != 1 {
			return usagef("usage: todo llm agent remove <name>")
		}
		if _, err := svc.UpdateSettings(func(s *llm.Settings) error {
			n := len(s.Agents)
			s.Agents = slices.DeleteFunc(s.Agents, func(x llm.AgentProfile) bool { return x.Name == pos[0] })
			if len(s.Agents) == n {
				return fmt.Errorf("%w: no agent %q", llm.ErrInvalid, pos[0])
			}
			return nil
		}); err != nil {
			return err
		}
	default:
		return usagef("todo llm agent: unknown action %q (list, add, remove)", verb)
	}
	st, err := svc.Settings()
	if err != nil {
		return err
	}
	if a.json {
		agents := st.Agents
		if agents == nil {
			agents = []llm.AgentProfile{}
		}
		return a.emitJSON(map[string]any{"agents": agents})
	}
	if len(st.Agents) == 0 {
		a.printf("没有配置智能体（todo llm agent add <名称> -- <命令> [参数]）\n")
	}
	for _, ag := range st.Agents {
		a.printf("%-12s %s", ag.Name, strings.Join(ag.Command, " "))
		if ag.Description != "" {
			a.printf("  — %s", ag.Description)
		}
		a.printf("\n")
	}
	return nil
}

func limitFlag(limit **int) func(*fset) {
	return func(f *fset) { *limit = f.Int("limit", 20, "how many entries") }
}

func llmCalls(a *app, args []string) error {
	var limit *int
	if _, err := a.subFlags("llm", "calls", "", limitFlag(&limit), args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	calls, err := svc.Calls(*limit)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"calls": calls})
	}
	for _, c := range calls {
		a.printf("%s  %-9s %-10s %s/%s  %s  %dms %s\n", a.localTime(c.CreatedAt), c.Purpose, c.SessionID, c.Profile, c.Model, c.Status, c.LatencyMS, c.Error)
	}
	return nil
}

func llmActions(a *app, args []string) error {
	var limit *int
	if _, err := a.subFlags("llm", "actions", "", limitFlag(&limit), args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	acts, err := svc.Actions(*limit)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"actions": acts})
	}
	for _, x := range acts {
		exit := ""
		if x.Result != nil {
			exit = "exit " + strconv.Itoa(x.Result.ExitCode)
		}
		if x.RunID != "" {
			exit = "run " + x.RunID
		}
		a.printf("%s  %s #%d %-7s %-9s %s  %s  %s\n", a.localTime(x.CreatedAt), x.SessionID, x.Item, x.Kind, x.Status, x.Actor,
			strings.Join(x.Request.Argv, " "), exit)
	}
	return nil
}
