Server Manager
- Runs on One main Server that has the Port 25565 for Minecraft accesible
- Manages Instance Manager to create Instances dynamically when needed and stops them when no plazyer is online (except for lobby)
- Starts a velocity proxy with the required Plugin with an HTTP API

Instance Manager
- Runs on each Server available
- Starts and stops Servers dynamically when requested
- When a Start is requested, downloads the newest Version of the World from GitHub
- Can restart Instances and Save Worlds when requested Manually


TO DO
- Web Interface ->
  - Proper CPU and RAM Usage of Instances Board
  - CPU/RAM NaN after Downtime
  - Plugin Update Button on each IM
