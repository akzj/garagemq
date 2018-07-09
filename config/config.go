package config

type Config struct {
	Users      []ConfigUser
	Tcp        TcpConfig
	Queue      Queue
	Db         Db
	Vhost      Vhost
	Security   Security
	Connection Connection
}

type ConfigUser struct {
	Username string
	Password string
}

type TcpConfig struct {
	Nodelay      bool
	ReadBufSize  int `yaml:"readBufSize"`
	WriteBufSize int `yaml:"writeBufSize"`
}

type Queue struct {
	ShardSize int `yaml:"shardSize"`
}

type Db struct {
	DefaultPath string `yaml:"defaultPath"`
	Engine      string `yaml:"engine"`
}

type Vhost struct {
	DefaultPath string `yaml:"defaultPath"`
}

type Security struct {
	PasswordCheck string `yaml:"passwordCheck"`
}

type Connection struct {
	ChannelsMax  uint16 `yaml:"channelsMax"`
	FrameMaxSize uint32 `yaml:"frameMaxSize"`
}
