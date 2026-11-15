package main

// 交付工具（deliver_result）：一个工具两个动作——①每个文件登记成交付卡片
// （用户拿到成果的正式入口，打开/下载/在文件夹中定位都以登记路径为准）；
// ②文件项可选带 upload（用户上传登记的原件路径），带了就同时登记「本轮
// 结束后由系统搬运过去」的作业。登记即时返回、不搬文件——执行发生在
// finishRun（本轮输出完成、状态转空闲）之后，模型在对话中永远看不到执行
// 结果，交付通道因此不构成绕过文件权限的路径；执行结果以 kind=report
// 注记落库，下一轮对话模型自然可见。
//
// 卡片路径跟着交付结果走（产品定义的核心语义）：搬运成功 → 该会话指向源
// 路径的交付登记改写为成品新位置（见 rewriteDeliveryPaths）；没登记搬运或
// 搬运失败 → 卡片保持原路径（源文件没动，卡片照常可打开）。
//
// 交付目标来自 uploads 表登记的原件路径（OrigPath）所在目录：桌面端把文件拖进
// 输入框、或用「＋→文件上传」选一次，都会登记原件真实路径，交付就是把成品送回
// 那个位置。要搬运必须显式指认是哪一次上传并显式给文件名：upload（编号或原件
// 路径）与 filename 是与门关系——只给其一在调用时就报错（不留到轮末执行才失败），
// 两者都不给则只登记交付卡片不搬运。查不到上传记录时给出可操作的原因（那种来源
// 交付回不了原处，如实说明而不是硬搬）。
//
// 搬运语义（对齐产品定义）：
//   - 移动：source 复制到目标位置后删除原处（删不掉只留双份，不算失败）；
//   - 快照：目标处若存在将被替代的同名文件，先全量备份到
//     <工作区>/.jxai/delivery-backups/<会话>/<时间戳>_<文件名> 再替换；
//   - 原子：目标同目录临时文件写好后 rename 替换（copyFileAtomic）。
//
// 手机端（Android）：上传不登记原件路径，「送回上传处」语义不成立——交付出口
// 统一为公有目录「下载」下的专属子目录（用户在文件管理器里直接可见），调用即
// 登记搬运作业，upload 参数不可用。

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// androidDeliveryDir 手机端交付出口：公有目录「下载」下的专属子目录
//（targetSdk 28 走 legacy storage，WRITE_EXTERNAL_STORAGE 运行时权限即可直写）。
const androidDeliveryDir = "/sdcard/Download/坚祥数字助理"

func deliverResultTool() Tool {
	// 手机端：交付出口固定为公有目录下载文件夹（不需要上传记录；目录建不出
	// 来多半是存储权限没给——把可操作的指引带给模型/用户，而不是裸错误）
	if isAndroidRuntime {
		return Tool{
			Spec: toolSpec("deliver_result",
				"登记本轮的最终交付文件并交付到手机的「下载」目录（/sdcard/Download/坚祥数字助理/，用户在系统文件管理器里直接可见）：每个文件先登记成交付卡片，本轮对话结束后系统再把它移动到下载目录，搬运成功后卡片自动改指下载目录里的成品。目标处若有同名文件会先快照备份到工作区 .jxai/delivery-backups/ 再替换，执行结果以系统注记进入后续对话。"+
					"用户要求「把文件发给我/放到下载目录」或产出成果文件时用本工具；工作区内常规写出不需要交付。若返回存储权限错误，请向用户说明：需要在系统弹窗或设置里允许本应用访问文件，然后重试。"+
					"调用本工具不会结束对话（对话结束的判定仍是：只输出思考和正文、没有工具调用），所以应在调用本工具之后再输出给用户的正文总结。",
				objSchema(map[string]any{
					"files": arrayProp("交付文件清单", map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":     strProp("文件路径（相对或绝对）"),
							"note":     strProp("一句话说明这个文件是什么（可选）"),
							"filename": strProp("交付到下载目录后的文件名（可选，缺省用源文件名）"),
						},
						"required": []string{"path"},
					}),
					"summary": strProp("本次交付的整体说明（可选）"),
				}, "files")),
			Run: runDeliverResult,
		}
	}
	return Tool{
		Spec: toolSpec("deliver_result",
			"登记本轮的最终交付文件：每个文件都会在用户界面展示交付卡片（可打开/下载），这是用户拿到成果的正式入口。产出成果文件（尤其是 docx/xlsx/pptx 等经程序生成的文档）后必须调用；只在回复里贴路径不算交付。每轮可多次调用，后登记的会并列展示。"+
				"可选搬运（与门参数）：文件项里 upload（上传注记里的编号如 u1，或该次上传的原件路径）与 filename（送回后的文件名）必须成对提供——只给其一调用立即报错、两者都不给＝只登记交付卡片不搬运；这是有意设计，搬运目标（位置+文件名）必须由你显式确认。两者齐备时，本轮对话结束后系统把成品移动到该上传的原件位置；目标处若有将被替代的同名文件，会先快照备份到工作区 .jxai/delivery-backups/ 下再完成替换；搬运成功后交付卡片自动改指原件位置，搬运失败则卡片保持指向工作区里的文件。upload 只接受本会话上传登记过的原件路径，不能随意填别的位置。"+
				"注意：用 write/edit 等文件工具在工作区内写出的常规改动【不需要】用本工具交付（系统本来就能跟踪那些改动）。调用本工具不会结束对话（对话结束的判定仍是：只输出思考和正文、没有工具调用），所以应在调用本工具之后再输出给用户的正文总结。",
			objSchema(map[string]any{
				"files": arrayProp("交付文件清单", map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":     strProp("文件路径（相对或绝对）"),
						"note":     strProp("一句话说明这个文件是什么（可选）"),
						"upload":   strProp("送回原件位置（与 filename 成对提供）：上传注记里的编号（如 u1）或该次上传的原件路径；与 filename 必须同时给或同时不给——只给其一调用立即报错，都不给＝只登记交付卡片不搬运"),
						"filename": strProp("送回时的文件名（与 upload 成对提供；纯文件名不带路径，两者必须同时给或同时不给）"),
					},
					"required": []string{"path"},
				}),
				"summary": strProp("本次交付的整体说明（可选）"),
			}, "files")),
		Run: runDeliverResult,
	}
}

// deliverItem 是一次 deliver_result 调用里通过全部校验的一个文件项：
// 第一阶段只校验不落库（任何一项校验失败整体报错，不产生半截登记），
// 第二阶段再统一登记卡片与搬运作业。
type deliverItem struct {
	abs     string // 工作区内成品（解析后绝对路径）
	display string // 卡片登记路径（displayPath：工作区内相对、区外绝对）
	note    string
	moveTo  string // 非空＝同时登记搬运作业，值为目标位置（展示用正斜杠）
	job     *DeliveryJob
}

func runDeliverResult(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Files []struct {
			Path     string `json:"path"`
			Note     string `json:"note"`
			Upload   string `json:"upload"`
			Filename string `json:"filename"`
		} `json:"files"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if len(p.Files) == 0 {
		return "", fmt.Errorf("files 不能为空")
	}
	// 搬运与门：upload（送回哪次上传的原件位置）与 filename（送回后的文件名）
	// 必须成对提供——搬运目标由模型显式确认，缺一个在调用时就报错，绝不落到
	// 轮末执行阶段才失败；两者都不给＝只登记交付卡片，不涉及搬运。
	for _, f := range p.Files {
		hasUp := strings.TrimSpace(f.Upload) != ""
		hasName := strings.TrimSpace(f.Filename) != ""
		if hasUp && !hasName {
			return "", fmt.Errorf("文件 %s 给了 upload 但缺 filename：搬运必须同时显式提供 upload（送回位置）与 filename（送回后的文件名），请补上 filename 后重试；或不带这对参数、只登记交付卡片", f.Path)
		}
		if hasName && !hasUp {
			return "", fmt.Errorf("文件 %s 给了 filename 但缺 upload：filename 只在搬运（送回上传原件位置）时有效，请补上 upload（上传注记编号如 u1 或原件路径）后重试；或去掉 filename 只登记交付卡片", f.Path)
		}
	}
	// 计划模式：登记卡片是无害展示，照常允许；upload 是搬运承诺，对齐原
	// deliver_file 的计划模式硬拦（登记即承诺搬运，计划模式下不该落任何实际行动）。
	if t.sess != nil && t.sess.PlanMode {
		for _, f := range p.Files {
			if strings.TrimSpace(f.Upload) != "" {
				return "", fmt.Errorf("计划模式下不能登记搬运（%s 带 upload）：请去掉 upload 只登记交付卡片，或等计划批准退出计划模式后再交付", f.Path)
			}
		}
	}
	// 纯卡片只写内部展示元数据，不动用户文件，故只读档也可登记。
	// Android 固定搬到下载目录；桌面端只有 upload 路径承诺搬运。
	for _, f := range p.Files {
		if isAndroidRuntime || strings.TrimSpace(f.Upload) != "" {
			if err := requireNotReadOnly(t, "交付搬运/回写原件"); err != nil {
				return "", err
			}
		}
	}
	// 远程会话：卡片登记照常（远程工作区文件，下载走远程读取）；搬运目标在
	// 本地上传目录，远程会话的工作区不在本机，搬不了。
	if t.isRemote() {
		for _, f := range p.Files {
			if strings.TrimSpace(f.Upload) != "" {
				return "", fmt.Errorf("upload（送回原件位置）仅支持本地会话：远程会话的工作区不在本机，无法搬运到本地上传目录")
			}
		}
	}
	// 第一阶段：逐项校验，全部通过才落库（不产生半截登记，模型重试无副作用）。
	items := []deliverItem{}
	missing := []string{}
	for _, f := range p.Files {
		abs, err := t.resolve(f.Path)
		if err != nil {
			return "", err
		}
		if _, err := t.authorize(permAction{Op: "read", Tool: "deliver_result", Path: abs}); err != nil {
			return "", err
		}
		if isDir, _, serr := t.envStat(abs); serr != nil || isDir {
			missing = append(missing, f.Path)
			continue
		}
		it := deliverItem{abs: abs, note: strings.TrimSpace(f.Note)}
		if isAndroidRuntime {
			name, err := sanitizeDeliveryName(f.Filename)
			if err != nil {
				return "", err
			}
			if name == "" {
				name = filepath.Base(abs)
			}
			if err := os.MkdirAll(androidDeliveryDir, 0755); err != nil {
				return "", fmt.Errorf("交付目录不可用：%v。多半是未授予存储权限——请向用户说明：在系统弹窗或 设置→应用→数字助理→权限 里允许「文件和媒体」访问后重试一次", err)
			}
			it.job = &DeliveryJob{
				SessionID: t.sess.ID, SourcePath: abs, TargetDir: androidDeliveryDir, TargetName: name,
				Status: "pending", CreatedAt: now(),
			}
			it.moveTo = filepath.ToSlash(filepath.Join(androidDeliveryDir, name))
			items = append(items, it)
			continue
		}
		if strings.TrimSpace(f.Upload) == "" {
			items = append(items, it)
			continue
		}
		// 搬运的东西必须是本会话工作区内的模型产物（卡片登记没有这个限制——
		// 总控登记子会话产出的文件时路径可能在别的工作区）。
		if !t.inWorkspace(abs) {
			return "", fmt.Errorf("待搬运文件必须位于工作区内：%s（工作区外的文件不属于可搬运的模型产物）", filepath.ToSlash(abs))
		}
		up, uerr := t.resolveUploadFor(f.Upload)
		if uerr != nil {
			return "", uerr
		}
		targetDir := filepath.Dir(up.OrigPath)
		// 与门校验已保证 filename 非空，这里只做清洗（拒绝路径分隔符等）
		name, err := sanitizeDeliveryName(f.Filename)
		if err != nil {
			return "", err
		}
		if di, err := os.Stat(targetDir); err != nil || !di.IsDir() {
			return "", fmt.Errorf("交付目标目录当前不存在：%s（用户可能移动或删除了该目录，请向用户说明）", filepath.ToSlash(targetDir))
		}
		it.job = &DeliveryJob{
			SessionID: t.sess.ID, SourcePath: abs, TargetDir: targetDir, TargetName: name,
			Status: "pending", CreatedAt: now(),
		}
		it.moveTo = fmt.Sprintf("%s（%s）", filepath.ToSlash(filepath.Join(targetDir, name)), up.UID)
		items = append(items, it)
	}
	if len(items) == 0 {
		return "", fmt.Errorf("以下文件不存在，未登记任何交付：%s", strings.Join(missing, "、"))
	}
	// 第二阶段：登记卡片（+ 可选搬运作业）。
	registered := []string{}
	moves := []string{}
	for _, it := range items {
		d := &Delivery{
			SessionID: t.sess.ID, Seq: t.seq, Path: t.displayPath(it.abs),
			Note: it.note, CreatedAt: now(),
		}
		if p.Summary != "" {
			d.Note = strings.TrimSpace(d.Note + "；" + p.Summary)
		}
		if err := t.app.Store.AddDelivery(d); err != nil {
			return "", fmt.Errorf("交付登记落库失败: %w", err)
		}
		registered = append(registered, d.Path)
		if it.job != nil {
			if err := t.app.Store.AddDeliveryJob(it.job); err != nil {
				return "", fmt.Errorf("交付登记落库失败: %w", err)
			}
			moves = append(moves, fmt.Sprintf("%s → %s", d.Path, it.moveTo))
		}
	}
	out := fmt.Sprintf("已登记 %d 个交付文件，用户界面已展示并提供下载：%s", len(registered), strings.Join(registered, "、"))
	if len(moves) > 0 {
		out += fmt.Sprintf("。其中 %d 项已登记轮末搬运（本轮因调用了工具不会结束对话，请继续完成正文输出）：%s。目标处若有将被替代的同名文件，会先快照备份到工作区 .jxai/delivery-backups/ 下再完成替换；搬运成功后对应交付卡片会自动改指新位置，失败则保持原路径",
			len(moves), strings.Join(moves, "；"))
		if pending := t.app.Store.PendingDeliveryCount(t.sess.ID); pending > 0 {
			out += fmt.Sprintf("。当前待搬运共 %d 项", pending)
		}
	}
	if len(missing) > 0 {
		out += "。以下文件不存在已跳过：" + strings.Join(missing, "、")
	}
	return out, nil
}

// findUpload 按模型给的引用解析上传记录：优先 uid（u1/u2…），其次按
// 原件路径（大小写不敏感、词法归一后比对）。只认登记了原件路径的记录。
func (t *toolContext) findUpload(ref string) *Upload {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	ups, err := t.app.Store.UploadsFor(t.sess.ID)
	if err != nil {
		return nil
	}
	lower := strings.ToLower(ref)
	for _, u := range ups {
		if u.OrigPath == "" {
			continue
		}
		if strings.EqualFold(u.UID, ref) {
			return u
		}
		if strings.EqualFold(u.OrigPath, ref) || strings.EqualFold(filepath.ToSlash(u.OrigPath), lower) {
			return u
		}
		if strings.EqualFold(canonicalFor(u.OrigPath), canonicalFor(ref)) {
			return u
		}
		// 也认工作区副本路径：模型常把注记里的工作区路径原样当作 upload 传
		// 回来，那条记录里的原件路径才是真正的交付目标。
		if strings.EqualFold(canonicalFor(u.CopyPath), canonicalFor(ref)) {
			return u
		}
	}
	return nil
}

// resolveUploadFor 解析本次交付要用的上传记录（交付目标的来源）。模型必须
// 显式给引用（与门：upload 与 filename 成对，缺一在参数校验就被拦下）：按
// uid / 原件路径 / 工作区副本路径解析，解析不了就报可读错误并列出可用编号。
// 不做按成品路径的自动反查——搬运目标（位置+文件名）必须由模型显式确认。
func (t *toolContext) resolveUploadFor(ref string) (*Upload, error) {
	ups, err := t.app.Store.UploadsFor(t.sess.ID)
	if err != nil {
		return nil, fmt.Errorf("读取上传记录失败: %v", err)
	}
	if u := t.findUpload(ref); u != nil {
		return u, nil
	}
	return nil, fmt.Errorf("找不到上传记录 %s；可用的编号与原件位置：%s", ref, uploadRefList(ups))
}

// uploadRefList 列出本会话可用的上传编号与原件位置（给模型纠错用）。
func uploadRefList(ups []*Upload) string {
	parts := []string{}
	for _, u := range ups {
		if u.OrigPath != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", u.UID, filepath.ToSlash(u.OrigPath)))
		}
	}
	if len(parts) == 0 {
		return "（本会话还没有登记过原件路径的上传）"
	}
	return strings.Join(parts, "；")
}

// sanitizeDeliveryName 清洗模型给的交付文件名：只取 basename、拒绝路径
// 分隔符与点目录；空串是合法返回（Android 端表示未指定、用源文件名；桌面端
// 与门校验保证到这里的 filename 必非空）。
func sanitizeDeliveryName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("交付文件名不能包含路径分隔符：%s（只给文件名，目录由上传记录决定）", name)
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("非法交付文件名：%s", name)
	}
	if len([]rune(name)) > 200 {
		return "", fmt.Errorf("交付文件名过长（>200 字符）")
	}
	return name, nil
}

// flushDeliveries 认领并执行交付作业。sessionID 为空串时处理全部会话
// （启动恢复用）；由 finishRun 异步调用，ClaimPendingJobs 的原子认领
// 保证并发 flush 不重不漏。
func (a *App) flushDeliveries(sessionID string) {
	jobs, err := a.Store.ClaimPendingJobs(sessionID)
	if err != nil {
		log.Printf("[交付] 认领待执行作业失败: %v", err)
		return
	}
	for _, j := range jobs {
		a.runDeliveryJob(j)
	}
}

func (a *App) runDeliveryJob(j *DeliveryJob) {
	j.ExecutedAt = now()
	m, err := a.Store.Get(j.SessionID)
	if err != nil {
		a.finishDeliveryJob(j, "failed", "无法读取交付会话权限，已放弃搬运")
		return
	}
	// 登记到轮末之间可能切回只读；恢复的旧作业也必须按最新档位复查。
	if err := requireNotReadOnly(&toolContext{app: a, sess: m}, "执行交付搬运/回写原件"); err != nil {
		a.finishDeliveryJob(j, "failed", err.Error())
		return
	}
	ws := wsOr(a, m)
	if m.Remote != nil {
		a.finishDeliveryJob(j, "failed", "远程会话不支持交付执行（工作区不在本机）")
		return
	}
	fail := func(msg string) {
		a.finishDeliveryJob(j, "failed", msg)
	}
	fi, err := os.Stat(j.SourcePath)
	if err != nil || fi.IsDir() {
		fail(fmt.Sprintf("待交付文件不存在或已被移动：%s（源文件不动的规则下无法交付，若模型已改名请重新登记）", filepath.ToSlash(j.SourcePath)))
		return
	}
	// 目标目录兜底重建（授权晚于登记、目录被清等场景），建不出才算失败
	_ = os.MkdirAll(j.TargetDir, 0755)
	if di, err := os.Stat(j.TargetDir); err != nil || !di.IsDir() {
		fail(fmt.Sprintf("交付目标目录不存在：%s", filepath.ToSlash(j.TargetDir)))
		return
	}
	target := filepath.Join(j.TargetDir, j.TargetName)
	// 目标处有将被替代的同名文件：先全量快照，快照失败就不动目标（宁可
	// 不交付也不能没有退路）
	if _, err := os.Stat(target); err == nil {
		backup, err := uniqueBackupPath(ws, j.SessionID, j.TargetName)
		if err == nil {
			err = copyFileAtomic(backup, target)
		}
		if err != nil {
			fail(fmt.Sprintf("快照备份失败，已放弃本次交付（目标文件未动）：%v（目标文件可能正被其他程序占用——如微信正在使用该文件，关闭后重试）", err))
			return
		}
		j.BackupPath = backup
	}
	if err := copyFileAtomic(target, j.SourcePath); err != nil {
		fail(fmt.Sprintf("搬运失败：%v（目标文件未被动过或仍为旧版。若报占用/拒绝访问，常见原因：目标文件正被其他程序占用（如微信在用它），或目标只读——文件本身带只读属性，或位于微信等软件的只读目录（微信接收文件的目录默认只读）；关闭占用程序、去掉只读或换个可写位置后可重试）", err))
		return
	}
	// 移动语义：目标就位后再删源文件；删不掉只是留了双份，不算失败
	if err := os.Remove(j.SourcePath); err != nil {
		log.Printf("[交付] 源文件删除失败（交付本身已成功，工作区留有副本）：%s: %v", j.SourcePath, err)
	}
	a.finishDeliveryJob(j, "done", "")
}

// finishDeliveryJob 回写作业终态、改写交付卡片路径并向会话注入模型可见的
// 执行结果注记。
func (a *App) finishDeliveryJob(j *DeliveryJob, status, errMsg string) {
	j.Status = status
	j.Error = errMsg
	_ = a.Store.UpdateDeliveryJob(j)
	target := filepath.ToSlash(filepath.Join(j.TargetDir, j.TargetName))
	var note string
	switch status {
	case "done":
		note = fmt.Sprintf("【交付结果】已把 %s 移动到 %s。", filepath.ToSlash(j.SourcePath), target)
		if j.BackupPath != "" {
			note += fmt.Sprintf("目标处原有同名文件，已被替代并快照备份到 %s。", filepath.ToSlash(j.BackupPath))
		} else {
			note += "目标处原无同名文件，未发生替代。"
		}
		// 卡片路径跟着交付结果走：文件已在新位置，本会话指向源路径的交付
		// 登记改写为成品新位置（打开/下载/定位都以登记路径为准）。
		a.rewriteDeliveryPaths(j, filepath.Join(j.TargetDir, j.TargetName))
	default:
		note = fmt.Sprintf("【交付结果】交付《%s》失败：%s。", j.TargetName, errMsg)
	}
	_ = a.Store.AppendRow(j.SessionID, &StoredMessage{
		Message: TextMessage("user", note), Kind: kindReport, Origin: "delivery", CreatedAt: now(),
	})
	if m, err := a.Store.Get(j.SessionID); err == nil {
		m.UpdatedAt = now()
		_ = a.Store.SaveMeta(m)
	}
	a.broadcast(Event{Type: "session_updated", SessionID: j.SessionID})
}

// rewriteDeliveryPaths 把本会话交付卡片里指向 job 源路径的登记行改写为成品
// 新位置：比对在解析后做（登记时可能是工作区相对路径），canonicalFor 归一
// + 大小写不敏感（Windows 路径）。失败只记日志——改写不了顶多卡片指向旧
// 位置（注记里有新位置可查），交付本身已完成，不该因它判失败。
func (a *App) rewriteDeliveryPaths(j *DeliveryJob, target string) {
	m, err := a.Store.Get(j.SessionID)
	if err != nil {
		return
	}
	tctx := &toolContext{app: a, sess: m}
	dvs, err := a.Store.DeliveriesFor(j.SessionID)
	if err != nil {
		return
	}
	src := canonicalFor(j.SourcePath)
	n := 0
	for _, d := range dvs {
		abs, err := tctx.resolve(d.Path)
		if err != nil {
			continue
		}
		if strings.EqualFold(canonicalFor(abs), src) {
			if err := a.Store.UpdateDeliveryPath(d.ID, filepath.ToSlash(target)); err == nil {
				n++
			}
		}
	}
	if n > 0 {
		log.Printf("[交付] 已把 %d 张交付卡片改指新位置：%s", n, filepath.ToSlash(target))
	}
}

// uniqueBackupPath 生成不冲突的快照备份路径：
// <工作区>/.jxai/delivery-backups/<会话>/<yyyyMMdd-HHmmss>_<文件名>，
// 同秒撞名时追加 -2、-3…。
func uniqueBackupPath(ws, sessionID, name string) (string, error) {
	dir := filepath.Join(ws, ".jxai", "delivery-backups", sessionID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	stamp := time.Now().Format("20060102-150405")
	p := filepath.Join(dir, stamp+"_"+name)
	for i := 2; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p, nil
		}
		p = filepath.Join(dir, fmt.Sprintf("%s_%s-%d", stamp, name, i))
	}
}
