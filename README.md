# ra2web-proxy

**ra2web-proxy** 是一个用于网页红警（品牌名 红色井界™）的边缘合规安全网关。它用于转发官方的 Chronodivide 客户端，并动态注入代码和内容，以满足区域合规要求。

## 功能

该项目的目的是提供一键启动网页红警代理转发网关的解决方案，使用户能够拥有自己的网页红警站点。

## 开始使用

要开始使用，请按照以下步骤进行操作：

1.使用 `go build` 命令构建项目。  
2.在您的机器上运行 `ra2web-proxy` 可执行文件。  
3.如果想要以 https 方式访问，参考 HTTPS 配置的部分。  

## 开发

要参与项目的开发，请按照以下步骤进行操作：

1. 将仓库克隆到本地。
2. 在您的主机（Host）文件中配置以下条目：

```
127.0.0.1 game.ra2web.cn
127.0.0.1 res.ra2web.cn
127.0.0.1 cn.ra2web.cn
```

3.以 /cmd/main.go 为入口启动工程，访问 http://game.ra2web.cn 即可。  
4.如果想要以 https 方式访问，参考 HTTPS 配置的部分。  


## HTTPS 配置

如果想要以 https 方式访问，需要先生成 SSL 证书:

```bash 
openssl genrsa -out cert.key 2048
openssl req -new -key cert.key -out cert.csr -subj "/CN=*.ra2web.cn"
openssl x509 -req -days 365 -in cert.csr -signkey cert.key -out cert.crt
```

然后修改 config/config.json，增加 https 配置：

```json
{
  "https": {
    "port": 443,
    "cert": "cert.crt",
    "key": "cert.key"
  }
}
```

> 注意：首次访问页面会报错，是因为自签证书导致域名 `cn.ra2web.cn` 和 `res.ra2web.cn` 不被浏览器信任，有两种解决方案：
> * 使用正式的证书。
> * 手动在浏览器访问这两个地址，让浏览器信任即可，只需要操作一次。  
>    * https://cn.ra2web.cn
>    * https://res.ra2web.cn

## 下一步计划

- [ ] 实现自动化的覆盖操作，例如 JSON 合并和配置文件 INI 合并。
- [ ] 添加可视化管理界面。
- [ ] 改进可观测性功能。
- [ ] 实现自动化的功能注入。

## 贡献

请直接提交issue或者是提交pull request，项目依然在持续迭代中
