export namespace host {
	
	export class LintIssue {
	    rule: string;
	    id?: string;
	    level: string;
	    step: number;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new LintIssue(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rule = source["rule"];
	        this.id = source["id"];
	        this.level = source["level"];
	        this.step = source["step"];
	        this.message = source["message"];
	    }
	}
	export class Step {
	    action: string;
	    url?: string;
	    field?: string;
	    selector?: string;
	    attr?: string;
	    prefix?: string;
	    default?: string;
	    find?: string;
	    replace?: string;
	    data?: Record<string, string>;
	    grab_form?: boolean;
	    name?: string;
	    purpose?: string;
	    value?: string;
	
	    static createFrom(source: any = {}) {
	        return new Step(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.action = source["action"];
	        this.url = source["url"];
	        this.field = source["field"];
	        this.selector = source["selector"];
	        this.attr = source["attr"];
	        this.prefix = source["prefix"];
	        this.default = source["default"];
	        this.find = source["find"];
	        this.replace = source["replace"];
	        this.data = source["data"];
	        this.grab_form = source["grab_form"];
	        this.name = source["name"];
	        this.purpose = source["purpose"];
	        this.value = source["value"];
	    }
	}
	export class Rule {
	    name: string;
	    domains: string[];
	    note?: string;
	    referer?: string;
	    steps: Step[];
	    enabled: boolean;
	    builtin: boolean;
	    id?: string;
	    added?: string;
	    origin?: string;
	
	    static createFrom(source: any = {}) {
	        return new Rule(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.domains = source["domains"];
	        this.note = source["note"];
	        this.referer = source["referer"];
	        this.steps = this.convertValues(source["steps"], Step);
	        this.enabled = source["enabled"];
	        this.builtin = source["builtin"];
	        this.id = source["id"];
	        this.added = source["added"];
	        this.origin = source["origin"];
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

export namespace main {
	
	export class ImportItem {
	    url: string;
	    status: string;
	    note?: string;
	
	    static createFrom(source: any = {}) {
	        return new ImportItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.status = source["status"];
	        this.note = source["note"];
	    }
	}
	export class ImportRecord {
	    at: string;
	    items: ImportItem[];
	
	    static createFrom(source: any = {}) {
	        return new ImportRecord(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.at = source["at"];
	        this.items = this.convertValues(source["items"], ImportItem);
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
	export class ImportResult {
	    posts: number;
	    threads: number;
	    dups: number;
	    failed: number;
	    message: string;
	    items: ImportItem[];
	
	    static createFrom(source: any = {}) {
	        return new ImportResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.posts = source["posts"];
	        this.threads = source["threads"];
	        this.dups = source["dups"];
	        this.failed = source["failed"];
	        this.message = source["message"];
	        this.items = this.convertValues(source["items"], ImportItem);
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

export namespace settings {
	
	export class Settings {
	    existingFiles: string;
	    perPostFolder: boolean;
	    showPreview: boolean;
	    minimizeTray: boolean;
	    soundOnDone: boolean;
	    shutdownOnDone: boolean;
	    downloadDir: string;
	    tempDir: string;
	    hostsDir: string;
	    postsConcurrent: number;
	    hostConcurrent: number;
	    maxRetries: number;
	    timeoutSec: number;
	    fetchOrigName: boolean;
	    fetchFloorTitle: boolean;
	    sortByForum: boolean;
	    forumDomain: string;
	    mirrors: string;
	    rateLimit: number;
	    username: string;
	    password: string;
	    autoLogin: boolean;
	    autoThank: boolean;
	    proxy: string;
	    proxyEnabled: boolean;
	    extBridgeEnabled: boolean;
	    extBridgePort: number;
	    winX: number;
	    winY: number;
	    winW: number;
	    winH: number;
	    sortCol: number;
	    sortDir: number;
	    threadSortCol: number;
	    threadSortDir: number;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.existingFiles = source["existingFiles"];
	        this.perPostFolder = source["perPostFolder"];
	        this.showPreview = source["showPreview"];
	        this.minimizeTray = source["minimizeTray"];
	        this.soundOnDone = source["soundOnDone"];
	        this.shutdownOnDone = source["shutdownOnDone"];
	        this.downloadDir = source["downloadDir"];
	        this.tempDir = source["tempDir"];
	        this.hostsDir = source["hostsDir"];
	        this.postsConcurrent = source["postsConcurrent"];
	        this.hostConcurrent = source["hostConcurrent"];
	        this.maxRetries = source["maxRetries"];
	        this.timeoutSec = source["timeoutSec"];
	        this.fetchOrigName = source["fetchOrigName"];
	        this.fetchFloorTitle = source["fetchFloorTitle"];
	        this.sortByForum = source["sortByForum"];
	        this.forumDomain = source["forumDomain"];
	        this.mirrors = source["mirrors"];
	        this.rateLimit = source["rateLimit"];
	        this.username = source["username"];
	        this.password = source["password"];
	        this.autoLogin = source["autoLogin"];
	        this.autoThank = source["autoThank"];
	        this.proxy = source["proxy"];
	        this.proxyEnabled = source["proxyEnabled"];
	        this.extBridgeEnabled = source["extBridgeEnabled"];
	        this.extBridgePort = source["extBridgePort"];
	        this.winX = source["winX"];
	        this.winY = source["winY"];
	        this.winW = source["winW"];
	        this.winH = source["winH"];
	        this.sortCol = source["sortCol"];
	        this.sortDir = source["sortDir"];
	        this.threadSortCol = source["threadSortCol"];
	        this.threadSortDir = source["threadSortDir"];
	    }
	}

}

export namespace store {
	
	export class Image {
	    id: number;
	    postRowId: number;
	    seq: number;
	    pageUrl: string;
	    thumbUrl: string;
	    directUrl: string;
	    originalName: string;
	    state: string;
	    fileSize: number;
	    filePath: string;
	    errorMsg: string;
	    dlMs: number;
	    host: string;
	
	    static createFrom(source: any = {}) {
	        return new Image(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.postRowId = source["postRowId"];
	        this.seq = source["seq"];
	        this.pageUrl = source["pageUrl"];
	        this.thumbUrl = source["thumbUrl"];
	        this.directUrl = source["directUrl"];
	        this.originalName = source["originalName"];
	        this.state = source["state"];
	        this.fileSize = source["fileSize"];
	        this.filePath = source["filePath"];
	        this.errorMsg = source["errorMsg"];
	        this.dlMs = source["dlMs"];
	        this.host = source["host"];
	    }
	}
	export class LogEntry {
	    id: number;
	    time: string;
	    level: string;
	    msg: string;
	
	    static createFrom(source: any = {}) {
	        return new LogEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.time = source["time"];
	        this.level = source["level"];
	        this.msg = source["msg"];
	    }
	}
	export class Post {
	    id: number;
	    threadId: number;
	    postId: number;
	    floorNo: number;
	    title: string;
	    postUrl: string;
	    hosts: string;
	    dir: string;
	    dirName: string;
	    forum: string;
	    mark: string;
	    status: string;
	    createdAt: string;
	    downloadedAt: string;
	    done: number;
	    failed: number;
	    total: number;
	
	    static createFrom(source: any = {}) {
	        return new Post(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.threadId = source["threadId"];
	        this.postId = source["postId"];
	        this.floorNo = source["floorNo"];
	        this.title = source["title"];
	        this.postUrl = source["postUrl"];
	        this.hosts = source["hosts"];
	        this.dir = source["dir"];
	        this.dirName = source["dirName"];
	        this.forum = source["forum"];
	        this.mark = source["mark"];
	        this.status = source["status"];
	        this.createdAt = source["createdAt"];
	        this.downloadedAt = source["downloadedAt"];
	        this.done = source["done"];
	        this.failed = source["failed"];
	        this.total = source["total"];
	    }
	}
	export class Thread {
	    id: number;
	    threadId: number;
	    title: string;
	    url: string;
	    forum: string;
	    postCount: number;
	    imgCount: number;
	    createdAt: string;
	    scanned: number;
	    mark: string;
	
	    static createFrom(source: any = {}) {
	        return new Thread(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.threadId = source["threadId"];
	        this.title = source["title"];
	        this.url = source["url"];
	        this.forum = source["forum"];
	        this.postCount = source["postCount"];
	        this.imgCount = source["imgCount"];
	        this.createdAt = source["createdAt"];
	        this.scanned = source["scanned"];
	        this.mark = source["mark"];
	    }
	}

}

