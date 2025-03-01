// Copyright (c) 2015-2023 CloudJ Technology Co., Ltd.

package services

import (
	"cloudiac/common"
	"cloudiac/configs"
	"cloudiac/portal/consts"
	"cloudiac/portal/consts/e"
	"cloudiac/portal/libs/db"
	"cloudiac/portal/models"
	"cloudiac/portal/models/resps"
	"cloudiac/portal/services/logstorage"
	"cloudiac/portal/services/notificationrc"
	"cloudiac/portal/services/vcsrv"
	"cloudiac/runner"
	"cloudiac/utils"
	"cloudiac/utils/kafka"
	"cloudiac/utils/logs"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/acarl005/stripansi"
	"github.com/alessio/shellescape"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

func GetTask(dbSess *db.Session, id models.Id) (*models.Task, e.Error) {
	task := models.Task{}
	err := dbSess.Where("id = ?", id).First(&task)
	if err != nil {
		if e.IsRecordNotFound(err) {
			return nil, e.New(e.TaskNotExists, err)
		}
		return nil, e.New(e.DBError, err)
	}

	return &task, nil
}

func DeleteTaskStep(tx *db.Session, taskId models.Id) e.Error {
	step := models.TaskStep{}
	_, err := tx.Where("task_id = ?", taskId).Delete(&step)
	if err != nil {
		return e.New(e.DBError, err)
	}
	return nil
}

// 删除环境下所有的偏移检测资源信息
func DeleteEnvResourceDrift(tx *db.Session, taskId models.Id) e.Error {
	drift := models.ResourceDrift{}
	_, err := tx.Where("res_id in (select id from iac_resource where task_id = ?)", taskId).Delete(&drift)
	if err != nil {
		return e.New(e.DBError, err)
	}
	return nil
}

// 删除已经手动恢复的资源
func DeleteEnvResourceDriftByAddressList(tx *db.Session, taskId models.Id, addressList []string) e.Error {
	drift := models.ResourceDrift{}
	_, err := tx.Where("res_id in (select id from iac_resource where task_id = ? and address not in (?))",
		taskId, addressList).Delete(&drift)
	if err != nil {
		return e.New(e.DBError, err)
	}
	return nil
}

func DeleteTask(tx *db.Session, taskId models.Id) e.Error {
	step := models.Task{}
	_, err := tx.Where("id = ?", taskId).Delete(&step)
	if err != nil {
		return e.New(e.DBError, err)
	}
	return nil
}

func DeleteHistoryDrfitCronTask(tx *db.Session, limit int) (int, e.Error) {
	taskIds := make([]models.Id, 0)
	taskQuery := tx.Model(&models.Task{}).Where(
		"end_at < DATE_SUB(NOW(), INTERVAL 7 DAY) AND is_drift_task = 1 AND `type` = ?", models.TaskTypePlan).
		Limit(limit)

	err := taskQuery.Pluck("id", &taskIds)
	if err != nil {
		return 0, e.New(e.DBError, fmt.Errorf("delete task error: %v", err))
	}

	// 删除任务以及任务相关的step
	num := int64(0)
	if len(taskIds) > 0 {
		if num, err = taskQuery.Unscoped().Delete(&models.Task{}); err != nil {
			return 0, e.New(e.DBError, err)
		}
		if _, err := tx.Where("task_id in (?)", taskIds).Delete(&models.TaskStep{}); err != nil {
			return 0, e.New(e.DBError, err)
		}
	}
	return int(num), nil
}

func GetResourceIdByAddressAndTaskId(sess *db.Session, address string, lastResTaskId models.Id) (*models.Resource, e.Error) {
	res := models.Resource{}
	if err := sess.Where("address = ? and task_id = ?", address, lastResTaskId).First(&res); err != nil {
		return nil, e.New(e.DBError, err)
	}
	return &res, nil
}

func CloneNewDriftTask(tx *db.Session, src models.Task, env *models.Env) (*models.Task, e.Error) {
	// logger := logs.Get().WithField("func", "CreateTask")
	// logger = logger.WithField("taskId", pt.Id)
	tpl, err := GetTemplateById(tx, src.TplId)
	if err != nil {
		return nil, e.New(e.InternalError, err)
	}

	// 克隆任务需要重置部分任务参数
	var (
		cronTaskType string
		taskSource   string
	)
	if env.AutoRepairDrift {
		cronTaskType = models.TaskTypeApply
		taskSource = consts.TaskSourceDriftApply
	} else {
		cronTaskType = models.TaskTypePlan
		taskSource = consts.TaskSourceDriftPlan
	}

	// 获取最新 repoAddr(带 token)，确保 vcs 更新后任务还可以正常 checkout 代码
	repoAddr, _, err := GetTaskRepoAddrAndCommitId(tx, tpl, src.Revision)
	if err != nil {
		return nil, e.AutoNew(err, e.InternalError)
	}

	task, er := newCommonTask(tpl, env, src)
	if er != nil {
		return nil, er
	}

	task.Name = common.CronDriftTaskName
	task.Type = cronTaskType
	task.IsDriftTask = true
	task.RepoAddr = repoAddr
	task.CommitId = src.CommitId
	task.CreatorId = consts.SysUserId
	task.AutoApprove = env.AutoApproval
	task.StopOnViolation = env.StopOnViolation
	// newCommonTask方法完成了对keyId赋值，这里不需要在进行一次赋值了
	//task.KeyId = env.KeyId
	task.Source = taskSource

	// 自动纠偏任务总是使用环境的最新部署通道配置
	task.RunnerId, er = GetAvailableRunnerIdByStr(env.RunnerId, env.RunnerTags)
	if er != nil {
		return nil, er
	}

	return doCreateTask(tx, *task, tpl, env)
}

func CreateTask(tx *db.Session, tpl *models.Template, env *models.Env, pt models.Task) (*models.Task, e.Error) {
	// logger := logs.Get().WithField("func", "CreateTask")
	// logger = logger.WithField("taskId", task.Id)
	task, er := newCommonTask(tpl, env, pt)
	if er != nil {
		return nil, er
	}

	var (
		err      error
		commitId string
	)
	task.RepoAddr, commitId, err = GetTaskRepoAddrAndCommitId(tx, tpl, task.Revision)
	if err != nil {
		return nil, e.New(e.InternalError, err)
	}
	if task.CommitId == "" {
		task.CommitId = commitId
	}

	// 最后再进行一次保底 runnerId 设置，如果任务有 runnerId 了则直接使用，否则通过环境的 runnerTags 获取 runnerId
	task.RunnerId, er = GetAvailableRunnerIdByStr(task.RunnerId, env.RunnerTags)
	if er != nil {
		return nil, er
	}
	return doCreateTask(tx, *task, tpl, env)
}

func newCommonTask(tpl *models.Template, env *models.Env, pt models.Task) (*models.Task, e.Error) {
	firstVal := utils.FirstValueStr
	task := models.Task{
		// 以下为需要外部传入的属性
		Name:            pt.Name,
		Targets:         pt.Targets,
		CreatorId:       pt.CreatorId,
		Variables:       pt.Variables,
		AutoApprove:     pt.AutoApprove,
		KeyId:           models.Id(firstVal(string(pt.KeyId), string(env.KeyId))),
		ExtraData:       pt.ExtraData,
		Revision:        firstVal(pt.Revision, env.Revision, tpl.RepoRevision),
		CommitId:        pt.CommitId,
		StopOnViolation: pt.StopOnViolation,

		RetryDelay:  utils.FirstValueInt(pt.RetryDelay, env.RetryDelay),
		RetryNumber: utils.FirstValueInt(pt.RetryNumber, env.RetryNumber),
		RetryAble:   utils.FirstValueBool(pt.RetryAble, env.RetryAble),

		// 以下值直接使用环境的配置(不继承模板的配置)
		OrgId:     env.OrgId,
		ProjectId: env.ProjectId,
		TplId:     env.TplId,
		EnvId:     env.Id,
		StatePath: env.StatePath,

		// 任务、环境工作目录为空，工作目录就应该为空，这里不需要在引用云模板的工作目录
		Workdir:   firstVal(pt.Workdir, env.Workdir),
		TfVersion: tpl.TfVersion,

		Playbook:     env.Playbook,
		TfVarsFile:   env.TfVarsFile,
		PlayVarsFile: env.PlayVarsFile,

		BaseTask: models.BaseTask{
			Type:        pt.Type,
			Pipeline:    pt.Pipeline,
			StepTimeout: utils.FirstValueInt(pt.StepTimeout, common.DefaultTaskStepTimeout),
			RunnerId:    firstVal(pt.RunnerId, env.RunnerId),

			Status:   models.TaskPending,
			Message:  "",
			CurrStep: 0,
		},
		Callback:    pt.Callback,
		Source:      pt.Source,
		SourceSys:   pt.SourceSys,
		IsDriftTask: pt.IsDriftTask,
	}
	task.Id = models.Task{}.NewId()
	return &task, nil
}

func doCreateTask(tx *db.Session, task models.Task, tpl *models.Template, env *models.Env) (*models.Task, e.Error) {
	// pipeline 内容可以从外部传入，如果没有传则尝试读取云模板目录下的文件
	var err error
	if er := createTaskParamCheck(task); er != nil {
		return nil, er
	}

	if task.Pipeline == "" {
		task.Pipeline, err = GetTplPipeline(tx, tpl.Id, task.Revision, task.Workdir)
		if err != nil {
			return nil, e.AutoNew(err, e.InvalidPipeline)
		}
	}

	pipeline, err := DecodePipeline(task.Pipeline)
	if err != nil {
		return nil, e.New(e.InvalidPipeline, err)
	}

	task.Flow = GetTaskFlowWithPipeline(pipeline, task.Type)
	steps := make([]models.TaskStep, 0)
	stepIndex := 0
	for _, pipelineStep := range task.Flow.Steps {
		taskStep, er := createTaskStep(tx, env, task, pipelineStep, stepIndex)
		if er != nil {
			return nil, er
		}
		if taskStep != nil {
			steps = append(steps, *taskStep)
			stepIndex += 1
		}
	}

	if len(steps) == 0 {
		return nil, e.New(e.TaskNotHaveStep, models.ErrTaskNoSteps)
	}

	if err = tx.Insert(&task); err != nil {
		return nil, e.New(e.DBError, errors.Wrapf(err, "save task"))
	}

	for i := range steps {
		if i+1 < len(steps) {
			steps[i].NextStep = steps[i+1].Id
		}
		if err = tx.Insert(&steps[i]); err != nil {
			return nil, e.New(e.DBError, errors.Wrapf(err, "save task step"))
		}
	}
	return &task, nil
}

// 创建任务参数检查
func createTaskParamCheck(task models.Task) e.Error {
	if task.Playbook != "" && task.KeyId == "" {
		return e.New(e.BadParam, fmt.Errorf("'keyId' is required to run playbook"))
	}
	if task.RepoAddr == "" {
		return e.New(e.BadParam, fmt.Errorf("'repoAddr' is required"))
	}
	if task.CommitId == "" {
		return e.New(e.BadParam, fmt.Errorf("'commitId' is required"))
	}
	if task.RunnerId == "" {
		return e.New(e.BadParam, fmt.Errorf("'runnerId' is required"))
	}
	return nil
}

func createTaskStep(tx *db.Session, env *models.Env, task models.Task, pipelineStep models.PipelineStep, stepIndex int) (*models.TaskStep, e.Error) {
	logger := logs.Get().
		WithField("func", "createTaskStep").
		WithField("task", task.Id).
		WithField("step", fmt.Sprintf("%s(%s)", pipelineStep.Type, pipelineStep.Name))

	if pipelineStep.Type == models.TaskStepPlay && task.Playbook == "" {
		logger.Infoln("not have playbook, skip this step")
		return nil, nil
	} else if pipelineStep.Type == models.TaskStepEnvScan || pipelineStep.Type == models.TaskStepOpaScan {
		// 如果环境扫描未启用，则跳过扫描步骤
		if !env.PolicyEnable {
			return nil, nil
		}
	}

	if len(task.Targets) != 0 && IsTerraformStep(pipelineStep.Type) {
		if pipelineStep.Type != models.TaskStepInit {
			for _, t := range task.Targets {
				pipelineStep.Args = append(pipelineStep.Args, fmt.Sprintf("-target=%s", shellescape.Quote(t)))
			}
		}
	}

	if pipelineStep.Type == models.TaskStepEnvScan || pipelineStep.Type == models.TaskStepOpaScan {
		// 对于包含扫描的任务，创建一个对应的 scanTask 作为扫描任务记录，便于后期扫描状态的查询
		scanTask := CreateMirrorScanTask(&task)
		if err := tx.Insert(scanTask); err != nil {
			return nil, e.New(e.DBError, err)
		}
		if err := InitScanResult(tx, scanTask); err != nil {
			return nil, e.New(e.DBError, errors.Wrapf(err, "task '%s' init scan result", task.Id))
		}
	}
	return newTaskStep(task, pipelineStep, stepIndex), nil
}

func GetTaskRepoAddrAndCommitId(tx *db.Session, tpl *models.Template, revision string) (string, string, e.Error) {
	repoInfo := &tplRepoInfo{
		User:     tpl.RepoUser,
		Addr:     tpl.RepoAddr,
		Token:    tpl.RepoToken,
		CommitId: "",
	}

	if tpl.VcsId == "" { // 用户直接填写的 repo 地址
		repoInfo.CommitId = revision
	} else {
		var er e.Error
		repoInfo, er = getTplRepoInfo(tx, tpl, revision)
		if er != nil {
			return "", "", er
		}
	}

	if repoInfo.Addr == "" {
		return "", "", e.New(e.BadParam, fmt.Errorf("repo address is blank"))
	}

	u, err := url.Parse(repoInfo.Addr)
	if err != nil {
		return "", "", e.New(e.InternalError, errors.Wrapf(err, "parse url: %v", repoInfo.Addr))
	} else if repoInfo.Token != "" {
		u.User = url.UserPassword(repoInfo.User, repoInfo.Token)
	}

	return u.String(), repoInfo.CommitId, nil
}

type tplRepoInfo struct {
	User     string
	Token    string
	Addr     string
	CommitId string
}

func getTplRepoInfo(tx *db.Session, tpl *models.Template, revision string) (*tplRepoInfo, e.Error) {
	var (
		vcs      *models.Vcs
		err      error
		repo     vcsrv.RepoIface
		repoInfo tplRepoInfo = tplRepoInfo{User: models.RepoUser}
	)

	if vcs, err = QueryVcsByVcsId(tpl.VcsId, tx); err != nil {
		if e.IsRecordNotFound(err) {
			return nil, e.New(e.VcsNotExists, err)
		}
		return nil, e.New(e.DBError, err)
	}

	vcsInstance, er := vcsrv.GetVcsInstance(vcs)
	if er != nil {
		return nil, e.New(e.VcsError, er)
	}

	if vcs.VcsType == models.VcsGitee {
		user, er := vcsInstance.UserInfo()
		if er != nil {
			return nil, e.New(e.VcsError, er)
		}
		repoInfo.User = user.Login
	}

	repo, er = vcsInstance.GetRepo(tpl.RepoId)
	if er != nil {
		return nil, e.New(e.VcsError, er)
	}

	repoInfo.CommitId, err = repo.BranchCommitId(revision)
	if err != nil {
		return nil, e.New(e.VcsError, er)
	}

	repoAddr := tpl.RepoAddr
	if repoAddr == "" {
		// 如果模板中没有记录 repoAddr，则动态获取
		repoAddr, er = vcsrv.GetRepoAddress(repo)
		if er != nil {
			return nil, e.New(e.VcsError, er)
		}
	} else if !strings.Contains(repoAddr, "://") {
		// 如果 addr 不是完整路径则添加上 vcs 的 address(这样可以允许保存相对路径到 repoAddr)
		repoAddr = utils.JoinURL(vcs.Address, repoAddr)
	}
	repoInfo.Addr = repoAddr

	repoToken := tpl.RepoToken
	if repoToken == "" {
		token, err := vcs.DecryptToken()
		if err != nil {
			return nil, e.New(e.VcsError, er)
		}
		repoToken = token
	}
	repoInfo.Token = repoToken

	return &repoInfo, nil
}

func ListPendingCronTask(tx *db.Session, envId models.Id) (bool, e.Error) {
	query := tx.Where("env_id = ? AND is_drift_task = 1 AND status IN (?)", envId,
		[]string{models.TaskPending, models.TaskRunning, models.TaskApproving})
	exist, err := query.Model(&models.Task{}).Exists()
	if err != nil {
		return exist, e.New(e.DBError, err)
	}
	return exist, nil
}

func GetTaskById(tx *db.Session, id models.Id) (*models.Task, e.Error) {
	o := models.Task{}
	if err := tx.Where("id = ?", id).First(&o); err != nil {
		if e.IsRecordNotFound(err) {
			return nil, e.New(e.TaskNotExists, err)
		}
		return nil, e.New(e.DBError, err)
	}
	return &o, nil
}

func QueryTask(query *db.Session) *db.Session {
	query = query.Model(&models.Task{})
	// 创建人姓名
	query = query.Joins("left join iac_user as u on u.id = iac_task.creator_id").
		LazySelectAppend("u.name as creator,iac_task.*")
	return query
}

var stepStatus2TaskStatusMap = map[string]string{
	// 步骤进入 pending，将任务标识为 running
	// (正常情况下步骤进入 pending 并不会触发 ChangeXXXStatus 调用，只有在步骤通过审批时会走到这个逻辑)
	models.TaskStepPending:   models.TaskRunning,
	models.TaskStepApproving: models.TaskApproving,
	models.TaskStepRejected:  models.TaskRejected,
	models.TaskStepRunning:   models.TaskRunning,
	models.TaskStepFailed:    models.TaskFailed,
	models.TaskStepTimeout:   models.TaskFailed,
	models.TaskStepAborted:   models.TaskAborted,
	models.TaskStepComplete:  models.TaskComplete,
}

func stepStatus2TaskStatus(s string) string {
	taskStatus, ok := stepStatus2TaskStatusMap[s]
	if !ok {
		panic(fmt.Errorf("unknown task step status '%v'", s))
	}
	return taskStatus
}

func ChangeTaskStatusWithStep(dbSess *db.Session, task models.Tasker, step *models.TaskStep) e.Error {
	switch t := task.(type) {
	case *models.Task:
		return ChangeTaskStatus(dbSess, t, stepStatus2TaskStatus(step.Status), step.Message, false)
	case *models.ScanTask:
		return ChangeScanTaskStatusWithStep(dbSess, t, step)
	default:
		panic("invalid task type on change task status with step, task" + task.GetId())
	}
}

func changeTaskStatusSetAttrs(dbSess *db.Session, task *models.Task, status, message string) models.Attrs {
	updateAttrs := models.Attrs{
		"message": message,
	}
	task.Message = message
	if status != "" {
		task.Status = status
		updateAttrs["status"] = status
	}
	now := models.Time(time.Now())
	if task.StartAt == nil && task.Started() {
		task.StartAt = &now
		updateAttrs["start_at"] = &now
	}
	if task.StartAt != nil && task.EndAt == nil && task.Exited() {
		task.EndAt = &now
		updateAttrs["end_at"] = &now
	}

	if task.Aborting && task.Exited() {
		// 任务已结束，清空 aborting 状态
		task.Aborting = false
		updateAttrs["aborting"] = false
	}
	return updateAttrs
}

// ChangeTaskStatus 修改任务状态(同步修改 StartAt、EndAt 等)，并同步修改 env 状态
// 该函数只修改以下字段:
// 	status, message, start_at, end_at, aborting
func ChangeTaskStatus(dbSess *db.Session, task *models.Task, status, message string, skipUpdateEnv bool) e.Error {
	preStatus := task.Status
	if preStatus == status && message == "" {
		return nil
	}

	updateAttrs := changeTaskStatusSetAttrs(dbSess, task, status, message)
	logger := logs.Get().WithField("taskId", task.Id)
	logger.Infof("change task to '%s'", status)
	logger.Debugf("update task attrs: %s", utils.MustJSON(updateAttrs))
	if _, err := dbSess.Model(task).Where("id = ?", task.Id).UpdateAttrs(updateAttrs); err != nil {
		return e.AutoNew(err, e.DBError)
	}

	if preStatus != status && !task.IsDriftTask &&
		// 忽略任务类型由 审批中 变更为 running 状态时的消息通知
		// running 状态变更为审批中时已经进行过通知了，这里就不需要在重复通知了
		!(preStatus == common.TaskApproving && status == common.TaskRunning) {
		TaskStatusChangeSendMessage(task, status)
	}

	defer func() {
		if task.Exited() {
			go taskStatusExitedCall(dbSess, task, status)
		}
	}()

	if !skipUpdateEnv {
		step, er := GetTaskStep(dbSess, task.Id, task.CurrStep)
		if er != nil {
			logs.Get().WithField("currStep", task.CurrStep).
				WithField("taskId", task.Id).Errorf("get task step error: %s", er)
			return e.AutoNew(er, e.DBError)
		}

		if err := ChangeEnvStatusWithTaskAndStep(dbSess, task.EnvId, task, step); err != nil {
			logs.Get().WithField("envId", task.EnvId).
				WithField("taskId", task.Id).Errorf("change env to status error: %s", err)
			return err
		}
	}

	return nil
}

// 当任务变为退出状态时执行的操作·
func taskStatusExitedCall(dbSess *db.Session, task *models.Task, status string) {
	if task.Type == common.TaskTypeApply || task.Type == common.TaskTypeDestroy {
		// 回调的消息通知只发送一次, 作业结束后发送通知
		SendKafkaMessage(dbSess, task, status)

		if task.Callback != "" {
			if utils.IsValidUrl(task.Callback) {
				go SendHttpMessage(task.Callback, dbSess, task, status)
			} else {
				logs.Get().Warnf("invalid task callback url: %s", task.Callback)
			}
		}

		syncManagedResToProvider(task)
	}

	// 如果勾选提交pr自动plan，任务结束时 plan作业结果写入PR评论中
	if task.Type == common.TaskTypePlan {
		SendVcsComment(dbSess, task, status)
	}
}

type TfState struct {
	FormVersion      string        `json:"form_version"`
	TerraformVersion string        `json:"terraform_version"`
	Values           TfStateValues `json:"values"`
}

// TfStateValues  doc: https://www.terraform.io/docs/internals/json-format.html#values-representation
type TfStateValues struct {
	Outputs      map[string]TfStateVariable `json:"outputs"`
	RootModule   TfStateModule              `json:"root_module"`
	ChildModules []TfStateModule            `json:"child_modules,omitempty"`
}

type TfStateModule struct {
	Address      string            `json:"address"`
	Resources    []TfStateResource `json:"resources"`
	ChildModules []TfStateModule   `json:"child_modules,omitempty"`
}

type TfStateVariable struct {
	Value     interface{} `json:"value"`
	Sensitive bool        `json:"sensitive,omitempty"`
}

type TfStateResource struct {
	ProviderName string      `json:"provider_name"`
	Address      string      `json:"address"`
	Mode         string      `json:"mode"` // managed、data
	Type         string      `json:"type"`
	Name         string      `json:"name"`
	Index        interface{} `json:"index"` // index 可以为整型或字符串
	DependsOn    []string    `json:"depends_on"`

	Values map[string]interface{} `json:"values"`
}

func UnmarshalStateJson(bs []byte) (*TfState, error) {
	state := TfState{}
	err := json.Unmarshal(bs, &state)
	return &state, err
}

func TraverseStateModule(module *TfStateModule) (rs []*models.Resource) {
	parts := strings.Split(module.Address, ".")
	moduleName := parts[len(parts)-1]
	for _, r := range module.Resources {
		idx := ""
		if r.Index != nil {
			idx = fmt.Sprintf("%v", r.Index)
		}
		rs = append(rs, &models.Resource{
			Provider:     r.ProviderName,
			Module:       moduleName,
			Address:      r.Address,
			Type:         r.Type,
			Name:         r.Name,
			Index:        idx,
			Attrs:        r.Values,
			Dependencies: r.DependsOn,
		})
	}
	for i := range module.ChildModules {
		rs = append(rs, TraverseStateModule(&module.ChildModules[i])...)
	}
	return rs
}

func SaveTaskResources(tx *db.Session, task *models.Task, values TfStateValues, proMap runner.ProviderSensitiveAttrMap, sensitiveKeys map[string][]string) error {

	bq := utils.NewBatchSQL(1024, "INSERT INTO", models.Resource{}.TableName(),
		"id", "org_id", "project_id", "env_id", "task_id", "provider", "module",
		"address", "type", "name", "index", "attrs", "sensitive_keys", "applied_at", "res_id", "dependencies")

	rs := make([]*models.Resource, 0)
	rs = append(rs, TraverseStateModule(&values.RootModule)...)
	for i := range values.ChildModules {
		rs = append(rs, TraverseStateModule(&values.ChildModules[i])...)
	}
	resources, err := GetResourceByEnvId(tx, task.EnvId)
	if err != nil {
		return err
	}
	resMap := SetResFieldsAsMap(resources)
	for _, r := range rs {
		if _, ok := r.Attrs["id"]; !ok {
			logs.Get().Warn("attrs key 'id' not exist")
		}
		resId := fmt.Sprintf("%v", r.Attrs["id"])
		if resId == "" {
			logs.Get().Warn("attrs key 'id' is null")
		}
		r.AppliedAt = models.Time(time.Now())
		if resMap != nil {
			if resMap[resId] != nil {
				r.AppliedAt = resMap[resId].(models.Time)
			}
		}
		if len(proMap) > 0 {
			providerKey := strings.Join([]string{r.Provider, r.Type}, "-")
			// 通过provider-type 拼接查找敏感词是否在proMap中
			if _, ok := proMap[providerKey]; ok {
				r.SensitiveKeys = proMap[providerKey]
			}
		}
		// sensitive values
		if keys, ok := sensitiveKeys[r.Address]; ok {
			r.Attrs = SensitiveAttrs(r.Attrs, keys, "")
		}

		err := bq.AddRow(models.NewId("r"), task.OrgId, task.ProjectId, task.EnvId, task.Id, r.Provider,
			r.Module, r.Address, r.Type, r.Name, r.Index, r.Attrs, r.SensitiveKeys, r.AppliedAt, resId, r.Dependencies)
		if err != nil {
			return err
		}
	}

	for bq.HasNext() {
		sql, args := bq.Next()
		if _, err := tx.Exec(sql, args...); err != nil {
			return err
		}
	}
	return nil
}

func SaveTaskOutputs(dbSess *db.Session, task *models.Task, vars map[string]TfStateVariable) error {
	task.Result.Outputs = make(map[string]interface{})
	for k, v := range vars {
		task.Result.Outputs[k] = v
	}
	if _, err := dbSess.Model(&models.Task{}).Where("id = ?", task.Id).
		UpdateColumn("result", task.Result); err != nil {
		return err
	}
	return nil
}

// TfPlan doc: https://www.terraform.io/docs/internals/json-format.html#plan-representation
type TfPlan struct {
	FormatVersion string `json:"format_version"`

	ResourceChanges []TfPlanResource `json:"resource_changes"`
}

type TfPlanResource struct {
	Address       string `json:"address"`
	ModuleAddress string `json:"module_address,omitempty"`
	ProviderName  string `json:"provider_name"`

	Mode  string      `json:"mode"` // managed、data
	Type  string      `json:"type"`
	Name  string      `json:"name"`
	Index interface{} `json:"index"`

	Change TfPlanResourceChange `json:"change"`
}

// TfPlanResourceChange doc: https://www.terraform.io/docs/internals/json-format.html#change-representation
type TfPlanResourceChange struct {
	Actions []string    `json:"actions"` // no-op, create, read, update, delete
	Before  interface{} `json:"before"`
	After   interface{} `json:"after"`
}

func UnmarshalPlanJson(bs []byte) (*TfPlan, error) {
	plan := TfPlan{}
	err := json.Unmarshal(bs, &plan)
	return &plan, err
}

func UnmarshalTfParseJson(bs []byte) (*resps.TfParse, error) {
	js := resps.TfParse{}
	err := json.Unmarshal(bs, &js)
	return &js, err
}

func SaveTaskChanges(dbSess *db.Session, task *models.Task, rs []TfPlanResource, isPlanResult bool, costs []float32, forecastFailed []string) error {

	var (
		resAdded     = 0
		resChanged   = 0
		resDestroyed = 0
	)
	for _, r := range rs {
		actions := r.Change.Actions
		switch {
		case utils.SliceEqualStr(actions, []string{"no-op"}):
			continue
		case utils.SliceEqualStr(actions, []string{"create", "delete"}):
			resAdded += 1
			resDestroyed += 1
		case utils.SliceEqualStr(actions, []string{"create"}):
			resAdded += 1
		case utils.SliceEqualStr(actions, []string{"update"}):
			resChanged += 1
		case utils.SliceEqualStr(actions, []string{"delete", "create"}):
			resDestroyed += 1
			resAdded += 1
		case utils.SliceEqualStr(actions, []string{"delete"}):
			resDestroyed += 1
		default:
			logs.Get().WithField("taskId", task.Id).Warnf("unknown plan change actions: %v", actions)
		}
	}
	if isPlanResult {
		task.PlanResult.ResAdded = &resAdded
		task.PlanResult.ResChanged = &resChanged
		task.PlanResult.ResDestroyed = &resDestroyed

		//  预估费用
		if len(costs) == 3 {
			task.PlanResult.ResAddedCost = &costs[0]
			task.PlanResult.ResDestroyedCost = &costs[1]
			task.PlanResult.ResUpdatedCost = &costs[2]
			task.PlanResult.ForecastFailed = forecastFailed
		}

		if _, err := dbSess.Model(&models.Task{}).Where("id = ?", task.Id).
			UpdateColumn("plan_result", task.PlanResult); err != nil {
			return err
		}
	} else {
		task.Result.ResAdded = &resAdded
		task.Result.ResChanged = &resChanged
		task.Result.ResDestroyed = &resDestroyed
		if _, err := dbSess.Model(&models.Task{}).Where("id = ?", task.Id).
			UpdateColumn("result", task.Result); err != nil {
			return err
		}
	}
	return nil
}

func GetTaskStepByStepId(tx *db.Session, stepId models.Id) (*models.TaskStep, error) {
	taskStep := models.TaskStep{}
	err := tx.Where("id = ?", stepId).First(&taskStep)
	if err != nil {
		if e.IsRecordNotFound(err) {
			return nil, e.New(e.TaskStepNotExists)
		}
		return nil, e.New(e.DBError, err)
	}
	return &taskStep, nil
}

func FetchTaskLog(ctx context.Context, task models.Tasker, stepType string, writer io.WriteCloser) (err error) {
	// close 后 read 端会触发 EOF error
	defer writer.Close()

	var (
		steps        []*models.TaskStep
		fetchedSteps = make(map[string]struct{})
	)

	steps, err = GetTaskSteps(db.Get(), task.GetId())
	if err != nil {
		return err
	}

	for {
		for _, s := range steps {
			if stepType != "" && s.Type != stepType {
				continue
			}
			if _, ok := fetchedSteps[s.Id.String()]; ok {
				continue
			}
			if err := fetchTaskStepLog(ctx, task, writer, s.Id); err != nil {
				return err
			}
			fetchedSteps[s.Id.String()] = struct{}{}
		}

		if task.Exited() {
			break
		}

		// 因为有 callback 步骤，所以任务的步骤是会新增的(但只加到末尾)。
		// 我们等待一定时间，确保没有新的步骤了才退出循环
		time.Sleep(consts.DbTaskPollInterval * 2)
		steps, err = GetTaskSteps(db.Get(), task.GetId())
		if err != nil {
			return err
		}
		if len(steps) <= len(fetchedSteps) {
			break
		}
	}

	return nil
}

func FetchTaskStepLog(ctx context.Context, task models.Tasker, writer io.WriteCloser, stepId models.Id) (err error) {
	// close 后 read 端会触发 EOF error
	defer writer.Close()
	return fetchTaskStepLog(ctx, task, writer, stepId)
}

func fetchTaskStepLog(ctx context.Context, task models.Tasker, writer io.Writer, stepId models.Id) (err error) {
	step, err := GetTaskStepByStepId(db.Get(), stepId)
	if err != nil {
		return err
	}

	storage := logstorage.Get()

	if task, step, err = waitTaskStepStarted(ctx, task, step.Index); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return nil
	default:
	}

	if step.IsExited() {
		var content []byte
		if content, err = storage.Read(step.LogPath); err != nil {
			if os.IsNotExist(err) {
				// 当前步骤没有日志文件
				return nil
			}
			return err
		} else if _, err = writer.Write(content); err != nil {
			return err
		}
	} else if step.IsStarted() { // running
		sleepDuration := consts.DbTaskPollInterval
		for {
			if err = fetchRunnerTaskStepLog(ctx, task.GetRunnerId(), step, writer); err != nil {
				if errors.Is(err, ErrRunnerTaskNotExists) && step.StartAt != nil &&
					time.Since(time.Time(*step.StartAt)) < consts.RunnerConnectTimeout*2 {
					// 某些情况下可能步骤被标识为了 running 状态，但调用 runner 执行任务时因为网络等原因导致没有及时启动执行。
					// 所以这里加一个判断, 如果是刚启动的任务会进行重试
					time.Sleep(sleepDuration)
					continue
				}
				return err
			}
			break
		}
	}

	return nil
}

func waitTaskStepStarted(ctx context.Context, tasker models.Tasker, stepIndex int) (task models.Tasker, step *models.TaskStep, err error) {
	sleepDuration := consts.DbTaskPollInterval
	ticker := time.NewTicker(sleepDuration)
	defer ticker.Stop()

	for {
		step, err = GetTaskStep(db.Get(), tasker.GetId(), stepIndex)
		if err != nil {
			return tasker, step, errors.Wrapf(err, "get task step %d", stepIndex)
		}

		if tasker.Exited() || step.IsStarted() {
			return tasker, step, nil
		}

		select {
		case <-ctx.Done():
			return tasker, step, nil
		case <-ticker.C:
			continue
		}
	}
}

var (
	ErrRunnerTaskNotExists = errors.New("runner task not exists")
)

func newReadMessageErr(err error) error {
	return errors.Wrap(err, "read message error")
}

// 从 runner 获取任务日志，直到任务结束
func fetchRunnerTaskStepLog(ctx context.Context, runnerId string, step *models.TaskStep, writer io.Writer) error {
	logger := logs.Get().WithField("func", "fetchRunnerTaskStepLog").
		WithField("taskId", step.TaskId).
		WithField("step", fmt.Sprintf("%d(%s)", step.Index, step.Type))

	runnerAddr, err := GetRunnerAddress(runnerId)
	if err != nil {
		return err
	}

	params := url.Values{}
	params.Add("envId", string(step.EnvId))
	params.Add("taskId", string(step.TaskId))
	params.Add("step", fmt.Sprintf("%d", step.Index))
	wsConn, resp, err := utils.WebsocketDail(runnerAddr, consts.RunnerTaskStepLogFollowURL, params)
	if err != nil {
		if resp != nil {
			if resp.StatusCode == http.StatusNotFound {
				return ErrRunnerTaskNotExists
			}
			respBody, _ := io.ReadAll(resp.Body)
			logger.Warnf("websocket dail error: %s, response: %s", err, respBody)
		}
		return errors.Wrapf(err, "websocket dail: %s/%s", runnerAddr, consts.RunnerTaskStepLogFollowURL)
	}

	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		_ = utils.WebsocketClose(wsConn)
	}()

	for {
		var reader io.Reader

		_, reader, err = wsConn.NextReader()
		if err != nil {
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseInternalServerErr) {
				logger.Traceln(newReadMessageErr(err))
				return nil
			} else {
				logger.Warnln(newReadMessageErr(err))
				return err
			}
		} else {
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			_, err = io.Copy(writer, reader)
			if err != nil {
				if errors.Is(err, io.ErrClosedPipe) {
					return nil
				}
				logger.Warnf("io copy error: %v", err)
				return err
			}
		}
	}
}

func TaskStatusChangeSendMessage(task *models.Task, status string) {
	// 非通知类型的状态直接跳过
	if _, ok := consts.TaskStatusToEventType[status]; !ok {
		logs.Get().WithField("taskId", task.Id).Infof("event don't need send message")
		return
	}
	dbSess := db.Get()
	env, _ := GetEnv(dbSess, task.EnvId)
	tpl, _ := GetTemplateById(dbSess, task.TplId)
	project, _ := GetProjectsById(dbSess, task.ProjectId)
	org, _ := GetOrganizationById(dbSess, task.OrgId)
	ns := notificationrc.NewNotificationService(&notificationrc.NotificationOptions{
		OrgId:     task.OrgId,
		ProjectId: task.ProjectId,
		Tpl:       tpl,
		Project:   project,
		Org:       org,
		Env:       env,
		Task:      task,
		EventType: consts.TaskStatusToEventType[status],
	})
	logs.Get().WithField("taskId", task.Id).Infof("new event: %s", ns.EventType)
	ns.SendMessage()
}

// ==================================================================================
// 扫描任务

// ChangeScanTaskStatus 修改扫描任务状态
// 该函数只更新以下字段:
//	"status", "policy_status", "message", "start_at", "end_at"
func ChangeScanTaskStatus(dbSess *db.Session, task *models.ScanTask, status, policyStatus, message string) e.Error {
	if task.Status == status && task.PolicyStatus == policyStatus && message == "" {
		return nil
	}

	updateAttrs := models.Attrs{
		"message": message,
	}
	task.Message = message
	if status != "" {
		task.Status = status
		updateAttrs["status"] = status
	}
	if policyStatus != "" {
		task.PolicyStatus = policyStatus
		updateAttrs["policy_status"] = policyStatus
	}
	now := models.Time(time.Now())
	if task.StartAt == nil && task.Started() {
		task.StartAt = &now
		updateAttrs["start_at"] = &now
	}
	if task.StartAt != nil && task.EndAt == nil && task.Exited() {
		task.EndAt = &now
		updateAttrs["end_at"] = &now
	}

	logger := logs.Get().WithField("taskId", task.Id)
	logger.Infof("change scan task to '%s'", status)
	logger.Debugf("update scan task attrs: %s", utils.MustJSON(updateAttrs))
	if _, err := dbSess.Model(task).Where("id = ?", task.Id).UpdateAttrs(updateAttrs); err != nil {
		return e.AutoNew(err, e.DBError)
	}

	return nil
}

func ChangeScanTaskStatusWithStep(dbSess *db.Session, task *models.ScanTask, step *models.TaskStep) e.Error {
	taskStatus := stepStatus2TaskStatus(step.Status)
	policyStatus := ""
	exitCode := step.ExitCode

	switch taskStatus {
	case common.TaskPending, common.TaskRunning:
		policyStatus = common.PolicyStatusPending
	case common.TaskComplete:
		policyStatus = common.PolicyStatusPassed
	case common.TaskFailed:
		if (step.Type == common.TaskStepEnvScan || step.Type == common.TaskStepTplScan || step.Type == common.TaskStepOpaScan) &&
			exitCode == common.TaskStepPolicyViolationExitCode {
			policyStatus = common.PolicyStatusViolated
		} else {
			policyStatus = common.PolicyStatusFailed
		}
	default: // "approving", "rejected", ...
		panic(fmt.Errorf("invalid scan task status '%s'", taskStatus))
	}
	return ChangeScanTaskStatus(dbSess, task, taskStatus, policyStatus, step.Message)
}

func CreateEnvScanTask(tx *db.Session, tpl *models.Template, env *models.Env, taskType string, creatorId models.Id) (*models.ScanTask, e.Error) {

	var (
		er  error
		err e.Error
	)

	// 使用默认 runner 执行扫描
	runnerId, err := GetDefaultRunnerId()
	if err != nil {
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}

	vars, er := GetValidVarsAndVgVars(tx, env.OrgId, env.ProjectId, env.TplId, env.Id)
	if er != nil {
		logs.Get().Debugf("get valid vars and vgVars error: %v", er)
		return nil, e.New(e.InternalError, er, http.StatusInternalServerError)
	}

	task := models.ScanTask{
		BaseTask: models.BaseTask{
			Type:        taskType,
			StepTimeout: common.DefaultTaskStepTimeout,
			RunnerId:    runnerId,
			Status:      models.TaskPending,
		},
		Name:         models.ScanTask{}.GetTaskNameByType(taskType),
		CreatorId:    creatorId,
		OrgId:        env.OrgId,
		TplId:        env.TplId,
		EnvId:        env.Id,
		ProjectId:    env.ProjectId,
		Revision:     env.Revision,
		Variables:    vars,
		Workdir:      tpl.Workdir,
		TfVersion:    tpl.TfVersion,
		TfVarsFile:   env.TfVarsFile,
		PlayVarsFile: env.PlayVarsFile,
		Playbook:     env.Playbook,
		ExtraData:    env.ExtraData,
		StatePath:    env.StatePath,
		PolicyStatus: common.PolicyStatusPending,
	}

	task.Id = task.NewId()

	task.RepoAddr, task.CommitId, err = GetTaskRepoAddrAndCommitId(tx, tpl, task.Revision)
	if err != nil {
		logs.Get().Debugf("get task repo addr and commit id failed: %v", err)
		return nil, e.New(e.InternalError, err)
	}

	task.Pipeline = models.DefaultPipelineRaw()
	pipeline := models.DefaultPipeline()

	task.Flow = GetTaskFlowWithPipeline(pipeline, task.Type)
	steps := make([]models.TaskStep, 0)
	stepIndex := 0
	for _, pipelineStep := range task.Flow.Steps {
		taskStep := newScanTaskStep(task, pipelineStep, stepIndex)
		steps = append(steps, *taskStep)
		stepIndex += 1
	}

	if len(steps) == 0 {
		return nil, e.New(e.TaskNotHaveStep, models.ErrTaskNoSteps)
	}

	if err := tx.Insert(&task); err != nil {
		return nil, e.New(e.DBError, errors.Wrapf(err, "save task"))
	}

	for i := range steps {
		if i+1 < len(steps) {
			steps[i].NextStep = steps[i+1].Id
		}
		if err := tx.Insert(&steps[i]); err != nil {
			return nil, e.New(e.DBError, errors.Wrapf(err, "save task step"))
		}
	}
	return &task, nil
}

func CreateScanTask(tx *db.Session, tpl *models.Template, env *models.Env, pt models.ScanTask) (*models.ScanTask, e.Error) {

	var (
		err error
		er  e.Error
	)
	envRevison := ""

	envId := models.Id("")
	if env != nil { // env != nil 表示为环境扫描任务
		tpl, er = GetTemplateById(tx, env.TplId)
		if er != nil {
			return nil, e.New(er.Code(), err, http.StatusBadRequest)
		}
		envId = env.Id
		envRevison = env.Revision
	}

	task := models.ScanTask{
		// 以下为需要外部传入的属性
		Name:      pt.Name,
		CreatorId: pt.CreatorId,
		ExtraData: pt.ExtraData,
		Revision:  utils.FirstValueStr(pt.Revision, envRevison, tpl.RepoRevision),

		OrgId:     tpl.OrgId,
		TplId:     tpl.Id,
		EnvId:     envId,
		ProjectId: pt.ProjectId,

		Workdir: tpl.Workdir,

		PolicyStatus: common.PolicyStatusPending,

		BaseTask: models.BaseTask{
			Type:        pt.Type,
			StepTimeout: utils.FirstValueInt(pt.StepTimeout, common.DefaultTaskStepTimeout),
			RunnerId:    pt.RunnerId,

			Status:   models.TaskPending,
			Message:  "",
			CurrStep: 0,
		},
	}

	task.Id = models.NewId("run")
	task.RepoAddr, task.CommitId, err = GetTaskRepoAddrAndCommitId(tx, tpl, task.Revision)
	if err != nil {
		return nil, e.New(e.InternalError, err)
	}

	{ // 参数检查
		if task.RepoAddr == "" {
			return nil, e.New(e.BadParam, fmt.Errorf("'repoAddr' is required"))
		}
		if task.CommitId == "" {
			return nil, e.New(e.BadParam, fmt.Errorf("'commitId' is required"))
		}
		if task.RunnerId == "" {
			return nil, e.New(e.BadParam, fmt.Errorf("'runnerId' is required"))
		}
	}

	task.Pipeline = models.DefaultPipelineRaw()
	pipeline := models.DefaultPipeline()

	task.Flow = GetTaskFlowWithPipeline(pipeline, task.Type)
	steps := make([]models.TaskStep, 0)
	stepIndex := 0

	for _, pipelineStep := range task.Flow.Steps {
		taskStep := newScanTaskStep(task, pipelineStep, stepIndex)
		steps = append(steps, *taskStep)
		stepIndex += 1
	}

	if len(steps) == 0 {
		return nil, e.New(e.TaskNotHaveStep, models.ErrTaskNoSteps)
	}

	if err := tx.Insert(&task); err != nil {
		return nil, e.New(e.DBError, errors.Wrapf(err, "save task"))
	}

	for i := range steps {
		if i+1 < len(steps) {
			steps[i].NextStep = steps[i+1].Id
		}
		if err := tx.Insert(&steps[i]); err != nil {
			return nil, e.New(e.DBError, errors.Wrapf(err, "save task step"))
		}
	}
	return &task, nil
}

func GetScanTaskById(tx *db.Session, id models.Id) (*models.ScanTask, e.Error) {
	o := models.ScanTask{}
	if err := tx.Where("id = ?", id).First(&o); err != nil {
		if e.IsRecordNotFound(err) {
			return nil, e.New(e.TaskNotExists, err)
		}
		return nil, e.New(e.DBError, err)
	}
	return &o, nil
}

// CreateMirrorScanTask 创建镜像扫描任务
func CreateMirrorScanTask(task *models.Task) *models.ScanTask {
	return &models.ScanTask{
		BaseTask:     task.BaseTask,
		OrgId:        task.OrgId,
		ProjectId:    task.ProjectId,
		TplId:        task.TplId,
		EnvId:        task.EnvId,
		Name:         task.Name,
		CreatorId:    task.CreatorId,
		RepoAddr:     task.RepoAddr,
		Revision:     task.Revision,
		CommitId:     task.CommitId,
		Workdir:      task.Workdir,
		Mirror:       true,
		MirrorTaskId: task.Id,
		Playbook:     task.Playbook,
		TfVarsFile:   task.TfVarsFile,
		TfVersion:    task.TfVersion,
		PlayVarsFile: task.PlayVarsFile,
		Variables:    task.Variables,
		StatePath:    task.StatePath,
		ExtraData:    task.ExtraData,
		PolicyStatus: common.TaskPending,
	}
}

// 查询任务所有的步骤信息
func QueryTaskStepsById(query *db.Session, taskId models.Id) *db.Session {
	return query.Model(&models.TaskStep{}).Where("task_id = ?", taskId).Order("`index`")
}

// 查询任务下某一个单独步骤的具体执行日志
func GetTaskStepLogById(tx *db.Session, stepId models.Id) ([]byte, e.Error) {
	query := tx.Joins("left join iac_task_step on iac_task_step.log_path=iac_storage.path").
		Where("iac_task_step.id = ?", stepId).
		LazySelectAppend("iac_storage.content")

	var dbStorage models.DBStorage
	if err := query.Find(&dbStorage); err != nil {
		return nil, e.New(e.DBError, err)
	}
	return dbStorage.Content, nil
}

func SendKafkaMessage(session *db.Session, task *models.Task, taskStatus string) {
	k := kafka.Get()
	if k == nil {
		return
	}

	resources := make([]models.Resource, 0)
	if err := session.Model(models.Resource{}).Where("org_id = ? AND project_id = ? AND env_id = ? AND task_id = ?",
		task.OrgId, task.ProjectId, task.EnvId, task.Id).Find(&resources); err != nil {
		logs.Get().Errorf("kafka send error, get resource data err: %v", err)
		return
	}

	var policyStatus string
	scanTask, err := GetScanTaskById(session, task.Id)
	if err != nil && err.Code() != e.TaskNotExists {
		logs.Get().Errorf("kafka send error, get scanTask data err: %v, taskId: %s", err, task.Id)
		return
	}

	outputs := make(map[string]interface{})
	for k, v := range task.Result.Outputs {
		m, ok := v.(map[string]interface{})
		if !ok {
			outputs[k] = v
			continue
		}

		if _, ok := m["sensitive"]; !ok {
			outputs[k] = v
			continue
		}

		m["value"] = "(sensitive value)"
		outputs[k] = m
	}

	if scanTask != nil {
		policyStatus = scanTask.PolicyStatus
	}

	env, err := GetEnvById(session, task.EnvId)
	if err != nil {
		logs.Get().Errorf("kafka send error, query env status err: %v", err)
		return
	}

	eventType := consts.DeployEventType
	result := kafka.InitIacKafkaCallbackResult()
	result.Resources = resources
	result.Outputs = outputs
	message := k.GenerateKafkaContent(task, eventType, taskStatus, env.Status, policyStatus, false, result)
	if err := k.ConnAndSend(message); err != nil {
		logs.Get().Errorf("kafka send error: %v", err)
		return
	}

	logs.Get().Infof("kafka send massage successful. TaskId: %s", task.Id)
}

func SendKafkaDriftMessage(session *db.Session, task *models.Task, isDrift bool,
	driftResources map[string]models.ResourceDrift) {

	k := kafka.Get()
	if k == nil {
		return
	}

	env, err := GetEnvById(session, task.EnvId)
	if err != nil {
		logs.Get().Errorf("kafka send error, query env status err: %v", err)
		return
	}

	eventType := consts.DriftEventType
	result := kafka.InitIacKafkaCallbackResult()
	result.DriftResources = driftResources
	message := k.GenerateKafkaContent(task, eventType, task.Status, env.Status, "", isDrift, result)
	if err := k.ConnAndSend(message); err != nil {
		logs.Get().Errorf("kafka send error: %v", err)
		return
	}

	logs.Get().Infof("kafka send massage successful. TaskId: %s", task.Id)
}

func SendHttpMessage(callbackUrl string, session *db.Session, task *models.Task, taskStatus string) {
	resources := make([]models.Resource, 0)
	if err := session.Model(models.Resource{}).Where("org_id = ? AND project_id = ? AND env_id = ? AND task_id = ?",
		task.OrgId, task.ProjectId, task.EnvId, task.Id).Find(&resources); err != nil {
		logs.Get().Errorf("send callback error, get resource data err: %v", err)
		return
	}

	var policyStatus string
	scanTask, err := GetScanTaskById(session, task.Id)
	if err != nil && err.Code() != e.TaskNotExists {
		logs.Get().Errorf("send callback error, get scanTask data err: %v, taskId: %s", err, task.Id)
		return
	}

	if scanTask != nil {
		policyStatus = scanTask.PolicyStatus
	}

	env, err := GetEnvById(session, task.EnvId)
	if err != nil {
		logs.Get().Errorf("send callback error, query env status err: %v", err)
		return
	}

	header := &http.Header{}
	header.Set("Content-Type", "application/json")
	message := GenerateCallbackContent(task, taskStatus, env.Status, policyStatus, resources)
	timeout := int(consts.CallbackTimeout.Seconds())
	if _, er := utils.HttpService(callbackUrl, "POST", header, message, timeout, timeout); er != nil {
		logs.Get().Warnf("send callback massage failed, err: %s, data: %+v", er, message)
		return
	}

	logs.Get().Infof("send callback massage successful. data: %+v", message)
}

type Resource struct {
	models.Resource
	DriftDetail string       `json:"driftDetail"`
	DriftAt     *models.Time `json:"driftAt"`
	IsDrift     bool         `json:"isDrift" form:"isDrift" `
}

func GetTaskResourceToTaskId(dbSess *db.Session, task *models.Task) ([]Resource, e.Error) {
	// 查询出最后一次漂移检测的资源
	// 资源类型: 新增、删除、修改
	rs := make([]Resource, 0)
	if err := dbSess.Table("iac_resource as r").
		Joins("left join iac_resource_drift as rd on rd.res_id =  r.id ").
		Where("r.org_id = ? AND r.project_id = ? AND r.env_id = ? AND r.task_id = ?",
			task.OrgId, task.ProjectId, task.EnvId, task.Id).
		LazySelectAppend("r.*, rd.drift_detail, rd.updated_at, rd.created_at").
		Find(&rs); err != nil {
		return nil, e.New(e.DBError, err)
	}

	return rs, nil
}

func GetTaskResourceCount(dbSess *db.Session, taskId models.Id) (int64, e.Error) {
	c, err := dbSess.Model(&models.Resource{}).Where("task_id = ?", taskId).Group("task_id").Count()
	if err != nil {
		return 0, e.AutoNew(err, e.DBError)
	}
	return c, nil
}

func InsertOrUpdateCronTaskInfo(session *db.Session, resDrift models.ResourceDrift) {
	dbResDrift := models.ResourceDrift{}
	if err := session.Where("res_id = ?", resDrift.ResId).Find(&dbResDrift); err != nil {
		logs.Get().Errorf("insert resource drift info error: %v", err)
		return
	}

	if dbResDrift.Id == "" {
		if err := models.Create(session, &resDrift); err != nil {
			logs.Get().Errorf("insert resource drift info error: %v", err)
		}
	} else {
		dbResDrift.DriftDetail = resDrift.DriftDetail
		if _, err := models.UpdateModelAll(session, &dbResDrift); err != nil {
			logs.Get().Errorf("update resource drift info error: %v", err)
		}
	}
}

func SendVcsComment(session *db.Session, task *models.Task, taskStatus string) {
	env, er := GetEnvById(session, task.EnvId)
	if er != nil {
		logs.Get().Errorf("vcs comment err, get env detail data err: %v", er)
		return
	}

	vp, err := GetVcsPrByTaskId(session, task)
	if err != nil {
		if !e.IsRecordNotFound(err) {
			logs.Get().Errorf("vcs comment err, get vcs pr data err: %v", err)
		}
		return
	}

	vcs, er := GetVcsRepoByTplId(session, task.TplId)
	if er != nil {
		logs.Get().Errorf("vcs comment err, get vcs data err: %v", er)
		return
	}
	taskStep, er := GetTaskPlanStep(session, task.Id)
	if er != nil {
		logs.Get().Errorf("vcs comment err, get task step data err: %v", er)
		return
	}

	logContent, err := logstorage.Get().Read(taskStep.LogPath)
	if err != nil {
		logs.Get().Errorf("vcs comment err, get task plan log err: %v", err)
		return
	}

	attr := map[string]interface{}{
		"Status": taskStatus,
		"Name":   env.Name,
		//http://{{addr}}/org/{{orgId}}/project/{{ProjectId}}/m-project-env/detail/{{envId}}/task/{{TaskId}}
		"Addr":    fmt.Sprintf("%s/org/%s/project/%s/m-project-env/detail/%s/task/%s", configs.Get().Portal.Address, task.OrgId, task.ProjectId, task.EnvId, task.Id),
		"Content": stripansi.Strip(string(logContent)),
	}

	content := utils.SprintTemplate(consts.PrCommentTpl, attr)
	if err := vcs.CreatePrComment(vp.PrId, content); err != nil {
		logs.Get().Errorf("vcs comment err, create comment err: %v", err)
		return
	}
}

func QueryResource(dbSess *db.Session, task *models.Task) *db.Session {
	return dbSess.Table("iac_resource as r").
		Joins("inner join iac_resource_drift as rd on rd.address =  r.address  and rd.env_id = ? ", task.EnvId).
		Where("r.org_id = ? AND r.project_id = ? AND r.env_id = ? AND r.task_id = ?",
			task.OrgId, task.ProjectId, task.EnvId, task.Id)
}

func GetDriftResource(session *db.Session, envId, driftTaskId models.Id) ([]models.ResourceDrift, e.Error) {
	driftResources := make([]models.ResourceDrift, 0)
	if err := session.Model(&models.ResourceDrift{}).
		Where("env_id = ?", envId).
		Where("task_id = ?", driftTaskId).
		Find(&driftResources); err != nil {
		return nil, e.New(e.DBError, err)
	}
	return driftResources, nil
}

type ResourceDriftResp struct {
	models.ResourceDrift
	IsDrift bool `gorm:"-" json:"isDrift"`
}

func GetDriftResourceById(session *db.Session, id string) (*ResourceDriftResp, e.Error) {
	driftResources := &ResourceDriftResp{}
	if err := session.Model(&models.ResourceDrift{}).
		Where("id = ?", id).
		First(driftResources); err != nil {
		return nil, e.New(e.DBError, err)
	}
	driftResources.IsDrift = true
	return driftResources, nil
}

func GetActiveTaskByEnvId(tx *db.Session, id models.Id) ([]models.Task, e.Error) {
	o := make([]models.Task, 0)
	if err := tx.Model(models.Task{}).
		Where("env_id = ?", id).
		Where("status in (?)", consts.TaskActiveStatus).
		Find(&o); err != nil {
		return nil, e.New(e.DBError, err)
	}
	return o, nil
}

func AbortRunnerTask(task models.Task) e.Error {
	return doAbortRunnerTask(task, false)
}

func CheckRunnerTaskCanAbort(task models.Task) e.Error {
	return doAbortRunnerTask(task, true)
}

func doAbortRunnerTask(task models.Task, justCheck bool) e.Error {
	logger := logs.Get().WithField("taskId", task.Id).WithField("action", "AbortTask")

	header := &http.Header{}
	header.Set("Content-Type", "application/json")

	var runnerAddr string
	runnerAddr, err := GetRunnerAddress(task.RunnerId)
	if err != nil {
		return e.AutoNew(err, e.InternalError)
	}

	requestUrl := utils.JoinURL(runnerAddr, consts.RunnerAbortTaskURL)
	logger.Debugf("request runner: %s", requestUrl)

	param := runner.TaskAbortReq{
		EnvId:     task.EnvId.String(),
		TaskId:    task.Id.String(),
		JustCheck: justCheck,
	}

	respData, err := utils.HttpService(requestUrl, "POST", header, param,
		int(consts.RunnerConnectTimeout.Seconds()),
		int(consts.RunnerConnectTimeout.Seconds())*10,
	)
	if err != nil {
		return e.AutoNew(err, e.RunnerError)
	}

	resp := runner.Response{}
	if err := json.Unmarshal(respData, &resp); err != nil {
		return e.New(e.RunnerError, fmt.Errorf("unexpected response: %s", respData))
	}
	logger.Debugf("runner response: %s", respData)

	if resp.Error != "" {
		return e.New(e.RunnerError, fmt.Errorf(resp.Error))
	}
	return nil
}

type CallbackResult struct {
	Resources []models.Resource `json:"resources"  `
}

type CallbackContent struct {
	EventType    string         `json:"eventType"`
	ExtraData    interface{}    `json:"extraData"`
	TaskStatus   string         `json:"taskStatus"`
	PolicyStatus string         `json:"policyStatus"`
	TaskType     string         `json:"taskType"`
	EnvStatus    string         `json:"envStatus"`
	OrgId        models.Id      `json:"orgId"`
	ProjectId    models.Id      `json:"projectId"`
	TplId        models.Id      `json:"tplId"`
	EnvId        models.Id      `json:"envId"`
	TaskId       models.Id      `json:"taskId"`
	Result       CallbackResult `json:"result"`
}

func GenerateCallbackContent(task *models.Task, taskStatus, envStatus, policyStatus string, resources []models.Resource) interface{} {
	a := CallbackContent{
		TaskStatus:   taskStatus,
		TaskType:     task.Type,
		PolicyStatus: policyStatus,
		EnvStatus:    envStatus,
		OrgId:        task.OrgId,
		ProjectId:    task.ProjectId,
		TplId:        task.TplId,
		EnvId:        task.EnvId,
		TaskId:       task.Id,
		Result: CallbackResult{
			Resources: resources,
		},
	}

	if task.ExtraData != nil {
		a.ExtraData = task.ExtraData
	} else {
		a.ExtraData = make(map[string]interface{})
	}

	return a
}
