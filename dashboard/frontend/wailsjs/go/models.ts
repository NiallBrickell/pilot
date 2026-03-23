export namespace main {
	
	export class LogEntry {
	    timestamp: string;
	    level: string;
	    source: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new LogEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.timestamp = source["timestamp"];
	        this.level = source["level"];
	        this.source = source["source"];
	        this.message = source["message"];
	    }
	}

}

export namespace pilot {
	
	export class PilotAction {
	    timestamp: string;
	    action_type: string;
	    detail: string;
	    confidence?: number;
	
	    static createFrom(source: any = {}) {
	        return new PilotAction(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.timestamp = source["timestamp"];
	        this.action_type = source["action_type"];
	        this.detail = source["detail"];
	        this.confidence = source["confidence"];
	    }
	}
	export class PilotPromptsConfig {
	    approval: string;
	    auto_respond: string;
	
	    static createFrom(source: any = {}) {
	        return new PilotPromptsConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.approval = source["approval"];
	        this.auto_respond = source["auto_respond"];
	    }
	}
	export class PilotGeneralConfig {
	    model: string;
	    confidence_threshold: number;
	    idle_timeout_ms: number;
	    pending_response_max_age_s: number;
	    grace_period_s: number;
	    escalation_timeout_s: number;
	    sse_port: number;
	    max_concurrent_evals: number;
	    evaluator_timeout_ms: number;
	    interrogation_confidence: number;
	
	    static createFrom(source: any = {}) {
	        return new PilotGeneralConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.model = source["model"];
	        this.confidence_threshold = source["confidence_threshold"];
	        this.idle_timeout_ms = source["idle_timeout_ms"];
	        this.pending_response_max_age_s = source["pending_response_max_age_s"];
	        this.grace_period_s = source["grace_period_s"];
	        this.escalation_timeout_s = source["escalation_timeout_s"];
	        this.sse_port = source["sse_port"];
	        this.max_concurrent_evals = source["max_concurrent_evals"];
	        this.evaluator_timeout_ms = source["evaluator_timeout_ms"];
	        this.interrogation_confidence = source["interrogation_confidence"];
	    }
	}
	export class PilotConfig {
	    general: PilotGeneralConfig;
	    prompts: PilotPromptsConfig;
	
	    static createFrom(source: any = {}) {
	        return new PilotConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.general = this.convertValues(source["general"], PilotGeneralConfig);
	        this.prompts = this.convertValues(source["prompts"], PilotPromptsConfig);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	
	export class PilotStats {
	    approvals_auto: number;
	    approvals_escalated: number;
	    auto_responses: number;
	    auto_responses_skipped: number;
	
	    static createFrom(source: any = {}) {
	        return new PilotStats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.approvals_auto = source["approvals_auto"];
	        this.approvals_escalated = source["approvals_escalated"];
	        this.auto_responses = source["auto_responses"];
	        this.auto_responses_skipped = source["auto_responses_skipped"];
	    }
	}
	export class PilotStatus {
	    available: boolean;
	    session_active: boolean;
	    session_start?: string;
	    stats: PilotStats;
	    recent_actions: PilotAction[];
	    has_pending_response: boolean;
	    hooks_installed: boolean;
	    wrapper_running: boolean;
	    sse_available: boolean;
	    sse_port: number;
	
	    static createFrom(source: any = {}) {
	        return new PilotStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.available = source["available"];
	        this.session_active = source["session_active"];
	        this.session_start = source["session_start"];
	        this.stats = this.convertValues(source["stats"], PilotStats);
	        this.recent_actions = this.convertValues(source["recent_actions"], PilotAction);
	        this.has_pending_response = source["has_pending_response"];
	        this.hooks_installed = source["hooks_installed"];
	        this.wrapper_running = source["wrapper_running"];
	        this.sse_available = source["sse_available"];
	        this.sse_port = source["sse_port"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

