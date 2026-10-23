package model

type BuildInfo struct {
	Version   string `json:"version"`
	BuildTime string `json:"build_time"`
	AppName   string `json:"app_name"`
}

type VersionInfo struct {
	Version   string `json:"version"`
	BuildTime string `json:"buildTime"`
	AppName   string `json:"appName"`
}

type HTTPError struct {
	Message string `json:"message"`
}
