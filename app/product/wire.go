package product

import "github.com/7ar-chat/harness/core/notify"
import "github.com/7ar-chat/harness/core/requirements"
import "github.com/7ar-chat/harness/core/roles"
import "github.com/7ar-chat/harness/core/schedule"
import "github.com/7ar-chat/harness/core/workflows"

// WireTypes returns a zero value of every JSON document this package's routes send to the web app, by the
// name of its TypeScript type. cmd/gen-ts reflects over them to generate web/src/api/generated.ts, so a new
// or changed view struct belongs here (a route that builds its answer from a map has no type to list: keep the answers named structs).
func WireTypes() map[string]any {
	return map[string]any{
		"User":                 userView{},
		"ProjectWithRole":      projectView{},
		"Member":               memberView{},
		"ProjectTemplate":      Template{},
		"PreviewInfo":          previewInfo{},
		"ProjectManifest":      Manifest{},
		"Thread":               threadView{},
		"ThreadSummary":        threadSummary{},
		"Overview":             overviewDoc{},
		"InboxDoc":             inboxDoc{},
		"InboxTask":            inboxTask{},
		"InboxThread":          inboxThread{},
		"InboxGate":            inboxGate{},
		"OverviewProject":      overviewProject{},
		"Connector":            connectorView{},
		"ConnectorPending":     linkPending{},
		"GithubRepo":           repoView{},
		"GithubStatus":         githubStatus{},
		"SupervisorSettings":   supervisorSettings{},
		"FeedEvent":            FeedEvent{},
		"MemoryEmbeddings":     memoryEmbeddings{},
		"MemoryEntry":          memoryEntryView{},
		"MemoryList":           memoryList{},
		"MemoryStats":          memoryStats{},
		"CapabilityView":       CapabilityView{},
		"CapabilityFieldView":  CapabilityFieldView{},
		"CapabilityTool":       CapabilityTool{},
		"CapabilityTestResult": CapabilityTestResult{},
		"SkillConnView":        SkillConnView{},
		"SkillFieldView":       SkillFieldView{},
		"SkillConnTest":        SkillConnTest{},
		"Requirement":          requirements.Requirement{},
		"RequirementSettings":  requirements.Settings{},
		"RequirementDetail":    RequirementDetail{},
		"RequirementImport":    RequirementImport{},
		"RoleView":             RoleView{},
		"RoleDoc":              roles.Role{}, // the document inside a RoleView (named so it does not collide with the Role enum)
		"ExecutorView":         ExecutorView{},
		"RoleVersion":          RoleVersion{},
		"RolePreview":          RolePreview{},
		"ScheduleView":         scheduleView{},
		"ScheduleRun":          schedule.Run{},
		"SchedulePreview":      schedulePreview{},
		"UsageView":            usageView{},
		"UsageCap":             capView{},
		"Workflow":             workflows.Workflow{},
		"WorkflowNode":         workflows.Node{},
		"WorkflowVersion":      workflows.Version{},
		"WorkflowTemplate":     workflows.Template{},
		"WorkflowRun":          workflows.Run{},
		"WorkflowRunDetail":    WorkflowRunDetail{},
		"WorkflowIssues":       WorkflowIssues{},
		"GitHostList":          gitHostList{},
		"GitHost":              gitHostView{},
		"GitCredential":        gitCredentialView{},
		"NotifyEvent":          notify.EventInfo{},
		"NotifyRules":          notify.Rules{},
		"NotifyRule":           notify.Rule{},
		"NotifyChannel":        NotificationChannel{},
		"NotifyView":           NotificationsView{},
		"NotifyTestResult":     NotificationTestResult{},
		"NotifyTestResults":    NotificationTestResults{},
		"NotifyLogEntry":       notify.LogEntry{},
		"MyNotifications":      MyNotifications{},
	}
}
