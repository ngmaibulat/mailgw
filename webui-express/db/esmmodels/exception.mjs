// import { Model, DataTypes } from "sequelize";
import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class Exception extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

Exception.init(
    {
        product: DataTypes.STRING,
        component: DataTypes.STRING,
        info: DataTypes.TEXT,
    },
    {
        sequelize,
        modelName: "Exception",
    }
);

export default Exception;
