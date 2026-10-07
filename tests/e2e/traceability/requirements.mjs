// Acceptance catalogue of the first release. Every test carries the tags
// of the items it verifies (`{ tag: ['@AT-06'] }`); the traceability
// reporter maps the results back to these items, the PRD references and the
// compatibility matrix. Deferred items are listed with their decision so
// the report shows them explicitly instead of silently missing.

export const ACCEPTANCE = [
  { id: 'AT-01', title: '自然语言创建任务（预览、追问一次、接受/撤销）', refs: ['FR-301', 'FR-303', 'FR-304', 'FR-306', 'FR-307', '验收 7.2', '验收 7.3'] },
  { id: 'AT-02', title: '大模型辅助决策（摘要、理由、差异、接受/撤销）', refs: ['FR-401', 'FR-402', 'FR-404', 'FR-406', '验收 7.4'] },
  { id: 'AT-03', title: '智能体启动、取消、失败与重试', refs: ['FR-501', 'FR-502', 'FR-504', 'FR-509', 'NFR-016', '验收 8.2', '验收 8.6'] },
  { id: 'AT-04', title: '提示词模板总结、复用与特殊字符完整性', refs: ['FR-507', 'NFR-039', '用户故事 9'] },
  { id: 'AT-05', title: '文本/文件/命令行/提交记录四类结果回写', refs: ['FR-508', 'FR-511', 'NFR-037', 'NFR-038', '验收 8.5', '验收 8.7'] },
  { id: 'AT-06', title: 'CLI 与网页双向同步 2 秒内可见', refs: ['FR-004', 'NFR-008', 'NFR-048', '验收 6.3'] },
  { id: 'AT-07', title: '并发冲突提示，不静默覆盖', refs: ['FR-004', 'NFR-009', '验收 6.4'] },
  { id: 'AT-08', title: '进程重启与崩溃后恢复', refs: ['FR-705', 'NFR-017', 'NFR-049', '验收 6.5'] },
  { id: 'AT-09', title: '导出导入与备份恢复', refs: ['NFR-020', 'NFR-030', 'NFR-049'] },
  { id: 'AT-10', title: '密钥与凭据脱敏', refs: ['FR-607', 'NFR-028', 'NFR-029', '验收 7.1', '验收 11.2'] },
  { id: 'AT-11', title: '权限撤销后不得继续执行', refs: ['FR-305', 'NFR-024', 'NFR-050', '验收 7.6'] },
  { id: 'AT-12', title: '模型不可用时基础任务可用', refs: ['FR-606', 'NFR-011', '验收 2.3'] },
  { id: 'AT-13', title: '终端键盘与鼠标交互（含无鼠标路径）', refs: ['FR-202', 'FR-205', 'FR-206', '验收 5.2', '验收 5.3'] },
  { id: 'AT-14', title: '浏览器兼容：Chromium / WebKit / Firefox 核心流程', refs: ['FR-005', 'NFR-003', 'NFR-047', '验收 3.1'] },
  { id: 'AT-15', title: '终端兼容：Terminal.app 与 iTerm2、UTF-8、无颜色模式', refs: ['NFR-004', 'NFR-047', '验收 3.2'] },
];

export const DEFERRED = [
  {
    id: '验收 9',
    title: '语音录入与补充',
    decision: '首版不纳入验收（FR-801，NFR-006）；首版以文本录入为准，后续版本复用现有任务录入流程（FR-802）。',
  },
];

// Compatibility matrix (default decision): macOS 14+, the latest two
// stable versions of Safari, Chrome, Edge and Firefox, Terminal and iTerm2.
// `projects` are the Playwright projects that run the branded product;
// without them the bundled engine (`engine`) is the evidence, and the report
// says so ("同引擎") instead of claiming the branded browser passed.
export const MATRIX = [
  { target: 'macOS 14+', kind: 'os', projects: [], note: '运行环境（报告记录实际 macOS 版本）' },
  { target: 'Safari（最新两个稳定版）', kind: 'browser', projects: [], engine: 'WebKit', note: 'Playwright WebKit 即 Safari 的引擎；品牌 Safari 的两个版本在 UAT 中人工确认' },
  { target: 'Chrome（最新两个稳定版）', kind: 'browser', projects: ['chrome'], engine: 'Chromium', note: 'TODO_E2E_CHANNELS=chrome 时使用本机安装的 Chrome；上一稳定版在安装了该版本的机器上同样运行' },
  { target: 'Edge（最新两个稳定版）', kind: 'browser', projects: ['msedge'], engine: 'Chromium', note: 'TODO_E2E_CHANNELS=msedge 时使用本机安装的 Edge；上一稳定版在安装了该版本的机器上同样运行' },
  { target: 'Firefox（最新两个稳定版）', kind: 'browser', projects: [], engine: 'Gecko', note: 'Playwright Firefox（Gecko 引擎）；上一稳定版在 UAT 中人工确认' },
  { target: 'Terminal.app', kind: 'terminal', projects: ['terminal'], profile: 'Terminal.app', note: 'node-pty 伪终端，按 Terminal.app 的 TERM/TERM_PROGRAM 与按键/鼠标序列' },
  { target: 'iTerm2', kind: 'terminal', projects: ['terminal'], profile: 'iTerm2', note: 'node-pty 伪终端，按 iTerm2 的 TERM/TERM_PROGRAM 与按键/鼠标序列' },
];

// QUALITY_GATE documents how model-dependent behaviour is accepted.
export const QUALITY_GATE = '大模型与智能体相关验收使用固定测试集（tests/e2e/support/mock-model.mjs）与固定 mock 智能体脚本（tests/e2e/agents/mock-agent.mjs），不要求真实外部模型在 CI 中可用。';
