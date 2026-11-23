package product

import "github.com/7ar-chat/harness/core/requirements"
import "github.com/7ar-chat/harness/core/roles"
import "github.com/7ar-chat/harness/core/schedule"

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
		"RoleVersion":          RoleVersion{},
		"RolePreview":          RolePreview{},
		"ScheduleView":         scheduleView{},
		"ScheduleRun":          schedule.Run{},
		"SchedulePreview":      schedulePreview{},
		"UsageView":            usageView{},
		"UsageCap":             capView{},
	}
}
