import base64

import requests
API = "http://127.0.0.1:8000/v1/files/references"
#file_path = "https://ts4.tc.mm.bing.net/th/id/OIP-C.mmBaX70dMVtLd7gkEcl11QHaJQ?r=0&rs=1&pid=ImgDetMain&o=7&rm=3"
#response = requests.post(API, json={"file_path": file_path, "is_image": True})
#print(response.status_code)
#print(response.text)
image = "/home/wkf/下载/R-C.jpeg"
base64_img = base64.b64encode(open(image, "rb").read()).decode("ascii")
data_url = f"data:image/jpeg;base64,{base64_img}"
print(data_url)
response = requests.post(API, json={"file_path": data_url, "is_image": True})
print(response.status_code)
print(response.text)




