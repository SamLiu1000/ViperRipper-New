package host

// The 16 image hosts ported from vripper-project 6.12.0 (Host.kt subclasses).
// Selectors are verbatim XPath from the original Kotlin classes. Steps mirror
// the original resolve() flows; url-derivations became regex steps.

var builtinRules = []Rule{
	{
		Name: "acidimg", Domains: []string{"acidimg.cc"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			// original checks for #continuebutton then always posts back
			{Action: "post", Source: "{page_url}", Data: map[string]string{"imgContinue": "Continue to your image"}},
			{Action: "extract", Field: "image_url", Selector: "//img[@class='centred']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[@class='centred']", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "dpic", Domains: []string{"dpic.me"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[@id='pic']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[@id='pic']", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "imagebam", Domains: []string{"imagebam.com"}, Enabled: true,
		Steps: []Step{
			{Action: "set-cookie", Name: "nsfw_inter", Value: "1"},
			{Action: "set-cookie", Name: "sfw_inter", Value: "1"},
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[contains(@class,'main-image')]", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[contains(@class,'main-image')]", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "imagetwist", Domains: []string{"imagetwist.com"}, Enabled: true, Referer: "https://imagetwist.com/",
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[contains(@class, 'img')]", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[contains(@class, 'img')]", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "imagevenue", Domains: []string{"imagevenue.com"}, Enabled: true,
		Steps: []Step{
			// original fetches twice when the interstitial is present; a second
			// GET is always harmless
			{Action: "get", Source: "{page_url}"},
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//a[@data-toggle='full']/img[@id='main-image']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//a[@data-toggle='full']/img[@id='main-image']", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "imagezilla", Domains: []string{"imagezilla.net"}, Enabled: true,
		Steps: []Step{
			// original: page URL with show→images IS the direct link
			{Action: "regex", Field: "image_url", Source: "{page_url}", Selector: "show", Replace: "images"},
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "filename", Selector: "//img[@id='photo']", Attr: "title", Default: "{url_name}"},
		},
	},
	{
		Name: "imgbox", Domains: []string{"imgbox.com"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[@id='img']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[@id='img']", Attr: "title", Default: "{url_name}"},
		},
	},
	{
		Name: "imgspice", Domains: []string{"imgspice.com"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[@id='imgpreview']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[@id='imgpreview']", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "imx", Domains: []string{"imx.to"}, Enabled: true,
		// Mirrors upstream vripper: the direct url derives from the THUMB
		// url (u/t → u/i keeps the date path the short /i/ link lacks), so
		// this rule requires {thumb_url}. No network needed to resolve.
		Steps: []Step{
			// seed image_url from the thumb (subsequent steps must NOT set
			// URL — they rewrite the field's current value)
			{Action: "regex", Field: "image_url", Source: "{thumb_url}",
				Selector: `^(.+)$`, Replace: "${1}"},
			{Action: "regex", Field: "image_url",
				Selector: `^https?://image\.imx\.to/u/t/(.+)$`,
				Replace:  "https://image.imx.to/u/i/${1}"},
			{Action: "regex", Field: "image_url",
				Selector: `^https?://imx\.to/u/t/(.+)$`,
				Replace:  "https://image.imx.to/u/i/${1}"},
			{Action: "regex", Field: "image_url",
				Selector: `^https?://t\.imx\.to/t/(.+)$`,
				Replace:  "https://image.imx.to/u/i/${1}"},
			{Action: "regex", Field: "image_url",
				Selector: `^https?://i\.imx\.to/t/(.+)$`,
				Replace:  "https://image.imx.to/u/i/${1}"},
			{Action: "regex", Field: "image_url",
				Selector: `^https?://imx\.to/upload/small/(.+)$`,
				Replace:  "https://image.imx.to/u/i/${1}"},
			// missing thumb: expandVars leaves the literal "{thumb_url}" —
			// blank it out so the rule fails instead of returning garbage
			{Action: "regex", Field: "image_url",
				Selector: `^\{thumb_url\}$`, Replace: ""},
		},
	},
	{
		Name: "pimpandhost", Domains: []string{"pimpandhost.com"}, Enabled: true,
		// original: strip -medium(.html), append size=original, then GET
		Steps: []Step{
			{Action: "regex", Field: "page_url", Source: "{page_url}", Selector: `-medium(\.html)?`, Replace: ""},
			{Action: "regex", Field: "page_url", Selector: `^([^?]*)$`, Replace: "${1}?size=original"},
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[contains(@class, 'original')]", Attr: "src", Prefix: "https:"},
			{Action: "extract", Field: "filename", Selector: "//img[contains(@class, 'original')]", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "pixhost", Domains: []string{"pixhost.to", "pixhost.cc"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[@id='image']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[@id='image']", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "pixroute", Domains: []string{"pixroute.com"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[@id='imgpreview']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[@id='imgpreview']", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "pixxxels", Domains: []string{"pixxxels.cc"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//*[@id='download']", Attr: "href"},
			{Action: "extract", Field: "filename", Selector: "/html/body/main/div/div[3]/div[1]/h6", Attr: "text", Default: "{url_name}"},
		},
	},
	{
		Name: "postimg", Domains: []string{"postimg.cc"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[contains(@class,'img-fluid')]", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[contains(@class,'img-fluid')]", Attr: "alt", Default: "{url_name}"},
		},
	},
	{
		Name: "turboimagehost", Domains: []string{"turboimagehost.com"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[@id='imageid']", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//div[contains(@class,'titleFullS')]/h1", Attr: "text", Default: "{url_name}"},
		},
	},
	{
		Name: "vipr", Domains: []string{"vipr.im"}, Enabled: true,
		Steps: []Step{
			{Action: "get", Source: "{page_url}"},
			{Action: "extract", Field: "image_url", Selector: "//img[contains(@class, 'img')]", Attr: "src"},
			{Action: "extract", Field: "filename", Selector: "//img[contains(@class, 'img')]", Attr: "alt", Default: "{url_name}"},
		},
	},
}
