import { agentsApi } from "./agents";
import { assetsApi } from "./assets";
import { auditApi } from "./audit";
import { authApi } from "./auth";
import { chatApi } from "./chat";
import { commandsApi } from "./commands";
import { companiesApi } from "./companies";
import { constraintsApi } from "./constraints";
import { conversationsApi } from "./conversations";
import { explorationApi } from "./exploration";
import { findingsApi } from "./findings";
import { goalsApi } from "./goals";
import { interceptApi } from "./intercept";
import { intranetApi } from "./intranet";
import { llmApi } from "./llm";
import { mcpApi } from "./mcp";
import { reportsApi } from "./reports";
import { scopeApi } from "./scope";
import { settingsApi } from "./settings";
import { skillsApi } from "./skills";
import { statsApi } from "./stats";
import { syncApi } from "./sync";
import { tasksApi } from "./tasks";
import { toolsApi } from "./tools";
import { trafficApi } from "./traffic";
import { visibilityApi } from "./visibility";
import { workspaceApi } from "./workspace";

export const api = {
  ...authApi,
  ...tasksApi,
  ...statsApi,
  ...goalsApi,
  ...constraintsApi,
  ...workspaceApi,
  ...assetsApi,
  ...companiesApi,
  ...explorationApi,
  ...findingsApi,
  ...auditApi,
  ...trafficApi,
  ...settingsApi,
  ...reportsApi,
  ...chatApi,
  ...llmApi,
  ...agentsApi,
  ...conversationsApi,
  ...toolsApi,
  ...mcpApi,
  ...syncApi,
  ...skillsApi,
  ...visibilityApi,
  ...interceptApi,
  ...scopeApi,
  ...commandsApi,
  ...intranetApi,
};
